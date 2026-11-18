package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"brood.dev/go/brood/pipeline"
)

// ErrSwarmChildDenied means the authenticated caller has no current child authority.
var ErrSwarmChildDenied = errors.New("swarm child authority denied")

// ErrSwarmChildConflict means the parent, immutable request, state, or revision differs.
var ErrSwarmChildConflict = errors.New("swarm child request or revision conflicts")

// MaxSwarmChildren bounds both a join's expected membership and its read snapshot.
const MaxSwarmChildren = 64

// SwarmChildPrincipal comes from a trusted authenticated adapter, never task arguments.
// Live native identity checks remain the adapter's responsibility.
type SwarmChildPrincipal struct {
	ProjectID, SwarmID, AtespaceUID, ActorUID string
	Incarnation                               int64
}

// SwarmChildParent pins the exact actor attempt and the first step of its pass.
type SwarmChildParent struct {
	Principal      SwarmChildPrincipal
	Pass, Sequence int64
	RequestID      string
}

// SwarmChildJoinPolicy is immutable and bounded. Quorum is positive only for
// quorum mode. Remaining must be retain: readiness never cancels active effects.
// Total is the exact expected membership; an incomplete group is never ready.
type SwarmChildJoinPolicy struct {
	Mode          string
	Total, Quorum int
	Remaining     string
}

// SwarmChildTask is durable intent, not authority to send a guest instruction.
// Only pending tasks can be cancelled. Running tasks survive process restarts
// unchanged: this store neither retries nor pretends to cancel external effects.
type SwarmChildTask struct {
	ID, RequestID, Payload         string
	Role, Delivery, DeliveryDetail string
	Executor                       SwarmChildPrincipal
	State, Outcome, Report         string
	Revision                       int64
	CreatedAt, FinishedAt          string
}

// SwarmChildJoinEvaluation reports readiness separately from outstanding work.
// Open includes both unfinished children and unregistered expected children.
// Even ready_success/ready_fail cannot release the parent while Open is nonzero.
type SwarmChildJoinEvaluation struct {
	State string
	Open  int
}

// SwarmChildJoin is a transactionally consistent snapshot, not a pipeline transition.
type SwarmChildJoin struct {
	Parent               SwarmChildParent
	Policy               SwarmChildJoinPolicy
	Children             []SwarmChildTask
	Evaluation           SwarmChildJoinEvaluation
	State                string
	Revision             int64
	AbortedBy, AbortedAt string
	// Interrupted is independent of late executor reports or delivery receipts.
	Interrupted bool
}

// SwarmChildAuthorization describes an exact operation for a local explicit policy.
// Caller is the parent for create/reserve/read/cancel, and the executor for
// start/complete. For abort, Caller is empty: HumanID and SessionHash identify
// the current owner/admin's web session, independently of worker authority.
// Neither a pipeline role nor parenthood grants child authority.
type SwarmChildAuthorization struct {
	Action               string
	Caller               SwarmChildPrincipal
	Parent               SwarmChildParent
	Executor             SwarmChildPrincipal
	RequestID, Payload   string
	HumanID, SessionHash string
}

// SwarmChildAuthorize checks current authenticated caller and scheduling rights
// in the write transaction. Nil denies. Use ctx for reads; do not perform network
// I/O, mutate state, or start a separate transaction. The store rechecks durable
// identities after this callback. These APIs do not delegate credentials/grants.
type SwarmChildAuthorize func(context.Context, SwarmChildAuthorization) error

func validChildPolicy(p SwarmChildJoinPolicy) bool {
	if p.Total < 1 || p.Total > MaxSwarmChildren || p.Remaining != "retain" {
		return false
	}
	switch p.Mode {
	case "all", "any":
		return p.Quorum == 0
	case "quorum":
		return p.Quorum >= 1 && p.Quorum <= p.Total
	}
	return false
}

// EvaluateSwarmChildJoin is pure. Cancelled children count as failures; invalid
// policies/states fail closed. Reports follow the existing actor-report bounds.
// It makes no graph decision and writes or cancels nothing.
func EvaluateSwarmChildJoin(p SwarmChildJoinPolicy, children []SwarmChildTask) (SwarmChildJoinEvaluation, error) {
	if !validChildPolicy(p) || len(children) > p.Total {
		return SwarmChildJoinEvaluation{}, ErrSwarmChildConflict
	}
	success, failed := 0, 0
	for _, c := range children {
		switch c.State {
		case "pending", "running", "cancelled":
			if c.Outcome != "" || c.Report != "" {
				return SwarmChildJoinEvaluation{}, ErrSwarmChildConflict
			}
			if c.State == "cancelled" {
				failed++
			}
		case "completed":
			switch c.Outcome {
			case "success":
				success++
			case "fail":
				failed++
			default:
				return SwarmChildJoinEvaluation{}, ErrSwarmChildConflict
			}
			if err := validStepReport(c.Report); err != nil {
				return SwarmChildJoinEvaluation{}, err
			}
		default:
			return SwarmChildJoinEvaluation{}, ErrSwarmChildConflict
		}
	}
	result := SwarmChildJoinEvaluation{State: "pending", Open: p.Total - success - failed}
	if len(children) != p.Total {
		return result, nil
	}
	required := p.Total
	if p.Mode == "any" {
		required = 1
	} else if p.Mode == "quorum" {
		required = p.Quorum
	}
	if success >= required {
		result.State = "ready_success"
	} else if success+result.Open < required {
		result.State = "ready_fail"
	}
	return result, nil
}

func validChildRequestID(id string) bool {
	return len(id) >= 1 && len(id) <= 128 && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") == ""
}

func (s *Store) childIdentity(ctx context.Context, p SwarmChildPrincipal) error {
	var allowed bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_actors a JOIN swarms sw ON sw.id=a.swarm_id
 WHERE sw.id=? AND sw.project_id=? AND sw.atespace_uid=? AND sw.state='active'
 AND a.uid=? AND a.atespace_uid=sw.atespace_uid AND a.incarnation=?
 AND a.phase='running' AND a.desired='running' AND a.stage='done' AND a.operation='start')`, p.SwarmID, p.ProjectID, p.AtespaceUID, p.ActorUID, p.Incarnation).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrSwarmChildDenied
	}
	return nil
}

func (s *Store) currentChildParent(ctx context.Context, p SwarmChildParent, evidence bool) error {
	if p.Pass < 1 || p.Sequence < p.Pass || !validPipelineField(p.RequestID) {
		return ErrSwarmChildConflict
	}
	if err := s.childIdentity(ctx, p.Principal); err != nil {
		return err
	}
	w, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	if err != nil {
		return err
	}
	stopped := !activeExecution(w.Execution)
	if stopped {
		interrupted, err := s.interruptedStep(ctx, w.ID, p.Sequence)
		if err != nil {
			return err
		}
		if !evidence || w.Execution != "stopped" || !interrupted {
			return ErrSwarmChildConflict
		}
	}
	v, err := s.GetSwarmStep(ctx, w.ID, p.Sequence)
	if err != nil {
		return err
	}
	var latest int64
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, w.ID).Scan(&latest); err != nil {
		return err
	}
	if latest != p.Sequence || v.Pass != p.Pass || v.ActorUID != p.Principal.ActorUID || v.ActorIncarnation != p.Principal.Incarnation || v.RequestID != p.RequestID {
		return ErrSwarmChildConflict
	}
	if v.State != "inflight" && v.State != "delivering" && !(v.State == "unconfirmed" && v.AttestedCommit != "") {
		return ErrSwarmChildConflict
	}
	_, _, commit, err := s.stepPass(ctx, w.ID, p.Sequence)
	if err != nil {
		return err
	}
	if commit == "" || v.AttestedCommit != commit {
		return ErrPipelineSourceUnverified
	}
	d, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return err
	}
	node, ok := d.Nodes[v.NodeID]
	if !ok || node.Type != "actor" {
		return ErrSwarmChildConflict
	}
	if stopped {
		// Историческое назначение уже проверено по immutable parent attempt;
		// новая привязка роли после stop не подменяет адресата старой задачи.
		return nil
	}
	return s.requireRoleActor(ctx, w.ID, node.Role, p.Principal.ActorUID, p.Principal.Incarnation)
}

func sameChildScope(a, b SwarmChildPrincipal) bool {
	return a.ProjectID == b.ProjectID && a.SwarmID == b.SwarmID && a.AtespaceUID == b.AtespaceUID
}

func (s *Store) authorizeChild(ctx context.Context, a SwarmChildAuthorization, authorize SwarmChildAuthorize) error {
	if authorize == nil {
		return ErrSwarmChildDenied
	}
	if err := authorize(ctx, a); err != nil {
		return err
	}
	evidence := a.Action == "read" || a.Action == "complete" || a.Action == "cancel"
	if err := s.currentChildParent(ctx, a.Parent, evidence); err != nil {
		return err
	}
	if a.Action == "reserve" || a.Action == "start" || a.Action == "complete" {
		if !sameChildScope(a.Parent.Principal, a.Executor) {
			return ErrSwarmChildDenied
		}
		return s.childIdentity(ctx, a.Executor)
	}
	return nil
}

// CreateSwarmChildJoin reserves immutable join membership for a current actor
// step. Exact retries reauthorize and return the existing snapshot. No child is
// executed, no role is rebound, and no graph is advanced by any child-store API.
func (s *Store) CreateSwarmChildJoin(ctx context.Context, p SwarmChildParent, policy SwarmChildJoinPolicy, authorize SwarmChildAuthorize) (SwarmChildJoin, error) {
	if !validChildPolicy(policy) {
		return SwarmChildJoin{}, ErrSwarmChildConflict
	}
	var result SwarmChildJoin
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.authorizeChild(ctx, SwarmChildAuthorization{Action: "create", Caller: p.Principal, Parent: p}, authorize); err != nil {
			return err
		}
		old, err := s.loadChildJoin(ctx, p)
		if err == nil {
			if old.Policy != policy {
				return ErrSwarmChildConflict
			}
			result = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_child_joins(swarm_id,parent_sequence,pass,project_id,atespace_uid,parent_uid,parent_incarnation,parent_request_id,mode,total,quorum,remaining,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.Principal.SwarmID, p.Sequence, p.Pass, p.Principal.ProjectID, p.Principal.AtespaceUID, p.Principal.ActorUID, p.Principal.Incarnation, p.RequestID, policy.Mode, policy.Total, policy.Quorum, policy.Remaining, nowRFC3339())
		if err != nil {
			return err
		}
		result, err = s.loadChildJoin(ctx, p)
		return err
	})
	if err != nil {
		return SwarmChildJoin{}, err
	}
	return result, nil
}

// ReserveSwarmChild records a bounded plain-text payload and exact executor under
// a stable parent-scoped request ID. Changed payload/executor conflicts, including
// after cancellation. The same request ID in another parent names another child.
func (s *Store) ReserveSwarmChild(ctx context.Context, p SwarmChildParent, requestID, payload string, executor SwarmChildPrincipal, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	return s.reserveSwarmChild(ctx, p, requestID, payload, "", executor, authorize)
}

func (s *Store) reserveSwarmChild(ctx context.Context, p SwarmChildParent, requestID, payload, role string, executor SwarmChildPrincipal, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	if !validChildRequestID(requestID) || strings.TrimSpace(payload) == "" || plainTextProblem(payload, MaxSwarmGoalBytes) != "" {
		return SwarmChildTask{}, ErrSwarmChildConflict
	}
	var result SwarmChildTask
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a := SwarmChildAuthorization{Action: "reserve", Caller: p.Principal, Parent: p, Executor: executor, RequestID: requestID, Payload: payload}
		if err := s.authorizeChild(ctx, a, authorize); err != nil {
			return err
		}
		join, err := s.loadChildJoin(ctx, p)
		if err != nil {
			return err
		}
		for _, old := range join.Children {
			if old.RequestID == requestID {
				if old.Payload != payload || old.Executor != executor || old.Role != role {
					return ErrSwarmChildConflict
				}
				result = old
				return nil
			}
		}
		if join.State != "open" || join.Interrupted || len(join.Children) >= join.Policy.Total {
			return ErrSwarmChildConflict
		}
		// Enqueue и mutation берут один BEGIN IMMEDIATE: проверка перед RPC
		// без этого встречного fence оставляла бы окно для новой задачи.
		if _, err := s.CurrentActorOperation(ctx, executor.ActorUID); err == nil {
			return newCoordinationConflict("child executor session operation is active or unconfirmed")
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		result = SwarmChildTask{ID: NewID(), RequestID: requestID, Payload: payload, Role: role, Executor: executor, State: "pending", Revision: 1, CreatedAt: nowRFC3339()}
		if role != "" {
			result.Delivery = "queued"
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_child_tasks(id,swarm_id,parent_sequence,request_id,payload,actor_uid,actor_incarnation,state,revision,created_at,role,delivery) VALUES(?,?,?,?,?,?,?,'pending',1,?,?,?)`, result.ID, p.Principal.SwarmID, p.Sequence, requestID, payload, executor.ActorUID, executor.Incarnation, result.CreatedAt, role, result.Delivery)
		return err
	})
	if err != nil {
		return SwarmChildTask{}, err
	}
	return result, nil
}

// GetSwarmChildJoin reauthorizes the parent and returns one bounded consistent
// snapshot. It is not an unauthenticated history API or permission to execute.
func (s *Store) GetSwarmChildJoin(ctx context.Context, p SwarmChildParent, authorize SwarmChildAuthorize) (SwarmChildJoin, error) {
	var result SwarmChildJoin
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.authorizeChild(ctx, SwarmChildAuthorization{Action: "read", Caller: p.Principal, Parent: p}, authorize); err != nil {
			return err
		}
		var err error
		result, err = s.loadChildJoin(ctx, p)
		return err
	})
	if err != nil {
		return SwarmChildJoin{}, err
	}
	return result, nil
}

func (s *Store) loadChildJoin(ctx context.Context, p SwarmChildParent) (SwarmChildJoin, error) {
	var j SwarmChildJoin
	j.Parent.Principal.SwarmID = p.Principal.SwarmID
	j.Parent.Sequence = p.Sequence
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT pass,project_id,atespace_uid,parent_uid,parent_incarnation,parent_request_id,mode,total,quorum,remaining,state,revision,COALESCE(aborted_by,''),COALESCE(aborted_at,'') FROM swarm_child_joins WHERE swarm_id=? AND parent_sequence=?`, p.Principal.SwarmID, p.Sequence).Scan(&j.Parent.Pass, &j.Parent.Principal.ProjectID, &j.Parent.Principal.AtespaceUID, &j.Parent.Principal.ActorUID, &j.Parent.Principal.Incarnation, &j.Parent.RequestID, &j.Policy.Mode, &j.Policy.Total, &j.Policy.Quorum, &j.Policy.Remaining, &j.State, &j.Revision, &j.AbortedBy, &j.AbortedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if j.Parent != p {
		return j, ErrSwarmChildConflict
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT id,request_id,payload,actor_uid,actor_incarnation,state,COALESCE(outcome,''),COALESCE(report,''),revision,created_at,COALESCE(finished_at,''),role,delivery,delivery_detail FROM swarm_child_tasks WHERE swarm_id=? AND parent_sequence=? ORDER BY rowid LIMIT 65`, p.Principal.SwarmID, p.Sequence)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	j.Children = []SwarmChildTask{}
	for rows.Next() {
		var c SwarmChildTask
		c.Executor = p.Principal
		if err := rows.Scan(&c.ID, &c.RequestID, &c.Payload, &c.Executor.ActorUID, &c.Executor.Incarnation, &c.State, &c.Outcome, &c.Report, &c.Revision, &c.CreatedAt, &c.FinishedAt, &c.Role, &c.Delivery, &c.DeliveryDetail); err != nil {
			return j, err
		}
		j.Children = append(j.Children, c)
	}
	if err := rows.Err(); err != nil {
		return j, err
	}
	j.Interrupted, err = s.interruptedStep(ctx, p.Principal.SwarmID, p.Sequence)
	if err != nil {
		return j, err
	}
	j.Evaluation, err = EvaluateSwarmChildJoin(j.Policy, j.Children)
	if err == nil && j.Interrupted {
		j.Evaluation.State = "unconfirmed"
	}
	if err == nil && j.State == "aborted" {
		if len(j.Children) >= j.Policy.Total {
			return j, ErrSwarmChildConflict
		}
		for _, c := range j.Children {
			if c.State != "cancelled" {
				return j, ErrSwarmChildConflict
			}
		}
		j.Evaluation = SwarmChildJoinEvaluation{State: "aborted"}
	}
	return j, err
}

// StartSwarmChild claims pending intent once by revision CAS. A repeated claim
// conflicts, even with the original revision: only one caller may act on success.
// A future dispatcher must commit the claim before effects, never send inside an
// outer WithTx, and reconcile running tasks rather than retry after a restart.
func (s *Store) StartSwarmChild(ctx context.Context, p SwarmChildParent, requestID string, caller SwarmChildPrincipal, expected int64, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	return s.transitionChild(ctx, p, requestID, caller, expected, "start", "", "", authorize)
}

// CompleteSwarmChild records one success/fail outcome and bounded report from
// the exact current executor. Exact retries use the original revision and bytes;
// stale incarnations, parents, passes and changed reports never affect history.
func (s *Store) CompleteSwarmChild(ctx context.Context, p SwarmChildParent, requestID string, caller SwarmChildPrincipal, expected int64, outcome, report string, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	if outcome != "success" && outcome != "fail" {
		return SwarmChildTask{}, ErrSwarmChildConflict
	}
	if err := validStepReport(report); err != nil {
		return SwarmChildTask{}, err
	}
	return s.transitionChild(ctx, p, requestID, caller, expected, "complete", outcome, report, authorize)
}

// CancelSwarmChild cancels only unstarted intent, explicitly and durably. A
// running effect cannot be labelled cancelled by this storage-only layer.
func (s *Store) CancelSwarmChild(ctx context.Context, p SwarmChildParent, requestID string, expected int64, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	return s.transitionChild(ctx, p, requestID, p.Principal, expected, "cancel", "", "", authorize)
}

func (s *Store) transitionChild(ctx context.Context, p SwarmChildParent, key string, caller SwarmChildPrincipal, expected int64, action, outcome, report string, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	if !validChildRequestID(key) || expected < 1 {
		return SwarmChildTask{}, ErrSwarmChildConflict
	}
	var result SwarmChildTask
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil {
			return ErrSwarmChildDenied
		}
		join, err := s.loadChildJoin(ctx, p)
		if err != nil {
			return err
		}
		var found bool
		for _, c := range join.Children {
			if c.RequestID == key {
				result = c
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
		wantCaller := result.Executor
		if action == "cancel" {
			wantCaller = p.Principal
		}
		if caller != wantCaller {
			return ErrSwarmChildDenied
		}
		a := SwarmChildAuthorization{Action: action, Caller: caller, Parent: p, Executor: result.Executor, RequestID: key, Payload: result.Payload}
		// Отмена не требует живого исполнителя: неотправленную задачу надо уметь
		// снять после его reset. Право и текущий родитель всё равно обязательны.
		if err := s.authorizeChild(ctx, a, authorize); err != nil {
			return err
		}
		if result.Role != "" && action != "cancel" {
			if err := s.requireRoleActor(ctx, p.Principal.SwarmID, result.Role, result.Executor.ActorUID, result.Executor.Incarnation); err != nil {
				return err
			}
			if result.Delivery == "not_sent" {
				return ErrSwarmChildConflict
			}
		}
		from, to := "pending", "running"
		switch action {
		case "complete":
			from, to = "running", "completed"
		case "cancel":
			to = "cancelled"
			if result.Delivery == "not_sent" {
				from = "running"
			}
		}
		if action != "start" && result.State == to && result.Revision == expected+1 && result.Outcome == outcome && result.Report == report {
			return nil
		}
		if join.State != "open" || result.State != from || result.Revision != expected {
			return ErrSwarmChildConflict
		}
		if action == "start" {
			if join.Interrupted {
				return newCoordinationConflict("owner interrupted this child group; reconcile before automatic scheduling")
			}
			if err := s.guardChildAllocation(ctx, result.Executor.ActorUID); err != nil {
				return err
			}
			if result.Role != "" {
				if err := s.childQueueHead(ctx, result); err != nil {
					return err
				}
				result.Delivery = "sending"
			}
		}
		if action == "complete" && result.Role != "" {
			result.Delivery = "delivered"
			result.DeliveryDetail = "The executor reported its outcome; receipt is proven independently of the delivery response."
		}
		finished := ""
		if to != "running" {
			finished = nowRFC3339()
		}
		update, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_child_tasks SET state=?,outcome=NULLIF(?,''),report=NULLIF(?,''),revision=revision+1,finished_at=NULLIF(?,''),delivery=?,delivery_detail=? WHERE id=? AND revision=? AND state=?`, to, outcome, report, finished, result.Delivery, result.DeliveryDetail, result.ID, expected, from)
		if err != nil {
			return err
		}
		n, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrSwarmChildConflict
		}
		result.State = to
		result.Outcome = outcome
		result.Report = report
		result.Revision++
		result.FinishedAt = finished
		return nil
	})
	if err != nil {
		return SwarmChildTask{}, err
	}
	return result, nil
}
