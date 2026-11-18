package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// SwarmChildWork is internal dispatcher input, not an authenticated guest view.
// Every effect must reauthorize and claim in its own committed transaction.
type SwarmChildWork struct {
	Parent        SwarmChildParent
	Task          SwarmChildTask
	QueuePosition int64
}

// EnqueueSwarmChild atomically establishes membership and pins a role's existing
// actor. An identical retry returns history without resetting delivery state.
func (s *Store) EnqueueSwarmChild(ctx context.Context, p SwarmChildParent, policy SwarmChildJoinPolicy, key, role, payload string, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	if len(role) < 1 || len(role) > 63 || strings.Trim(role, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || strings.HasPrefix(role, "-") || strings.HasSuffix(role, "-") {
		return SwarmChildTask{}, ErrSwarmChildConflict
	}
	var result SwarmChildTask
	err := s.WithTx(ctx, func(ctx context.Context) (err error) {
		// WithTx deliberately reuses an outer transaction without savepoints.
		// This compound operation must undo a new join even when its caller
		// handles the child's refusal and commits that outer transaction.
		if _, err = s.conn(ctx).ExecContext(ctx, `SAVEPOINT swarm_child_enqueue`); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err != nil {
				_, rollback := s.conn(cleanup).ExecContext(cleanup, `ROLLBACK TO swarm_child_enqueue`)
				err = errors.Join(err, rollback)
			}
			_, release := s.conn(cleanup).ExecContext(cleanup, `RELEASE swarm_child_enqueue`)
			err = errors.Join(err, release)
		}()
		binding, err := s.ResolveSwarmRole(ctx, p.Principal.SwarmID, role)
		if err != nil {
			return err
		}
		executor := p.Principal
		executor.ActorUID = binding.ActorUID
		executor.Incarnation = binding.ActorIncarnation
		if executor.ActorUID == p.Principal.ActorUID {
			return ErrSwarmChildDenied
		}
		if _, err = s.CreateSwarmChildJoin(ctx, p, policy, authorize); err != nil {
			return err
		}
		result, err = s.reserveSwarmChild(ctx, p, key, payload, role, executor, authorize)
		if err != nil {
			return err
		}
		// Policies can read current rights but cannot turn a stale role into a claim.
		return s.requireRoleActor(ctx, p.Principal.SwarmID, role, executor.ActorUID, executor.Incarnation)
	})
	if err != nil {
		return SwarmChildTask{}, err
	}
	return result, nil
}

func (s *Store) childQueueHead(ctx context.Context, c SwarmChildTask) error {
	var head string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT id FROM swarm_child_tasks WHERE actor_uid=? AND state='pending' AND delivery='queued' ORDER BY rowid LIMIT 1`, c.Executor.ActorUID).Scan(&head)
	if err != nil {
		return err
	}
	if head != c.ID {
		return newCoordinationConflict("an earlier child is waiting for this executor allocation")
	}
	return nil
}

// SwarmChildParentAt reads a historical parent for a trusted server adapter.
// It grants no scheduling, recovery, or guest read authority.
func (s *Store) SwarmChildParentAt(ctx context.Context, swarm string, sequence int64) (SwarmChildParent, error) {
	var p SwarmChildParent
	p.Principal.SwarmID = swarm
	p.Sequence = sequence
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT project_id,atespace_uid,parent_uid,parent_incarnation,pass,parent_request_id FROM swarm_child_joins WHERE swarm_id=? AND parent_sequence=?`, swarm, sequence).Scan(&p.Principal.ProjectID, &p.Principal.AtespaceUID, &p.Principal.ActorUID, &p.Principal.Incarnation, &p.Pass, &p.RequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// SwarmChildWorkByID reads internal history. Adapters must compare the full
// authenticated principal before returning any private task or report.
func (s *Store) SwarmChildWorkByID(ctx context.Context, id string) (SwarmChildWork, error) {
	var result SwarmChildWork
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var swarm string
		var sequence, position int64
		err := s.conn(ctx).QueryRowContext(ctx, `SELECT swarm_id,parent_sequence,rowid FROM swarm_child_tasks WHERE id=?`, id).Scan(&swarm, &sequence, &position)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		p, err := s.SwarmChildParentAt(ctx, swarm, sequence)
		if err != nil {
			return err
		}
		j, err := s.loadChildJoin(ctx, p)
		if err != nil {
			return err
		}
		for _, c := range j.Children {
			if c.ID == id {
				result = SwarmChildWork{Parent: p, Task: c, QueuePosition: position}
				return nil
			}
		}
		return ErrNotFound
	})
	return result, err
}

// QueuedSwarmChildren returns at most 128 FIFO executor heads. Busy executors
// do not hide other workers behind a queue of tasks for one allocation.
func (s *Store) QueuedSwarmChildren(ctx context.Context) ([]SwarmChildWork, error) {
	return s.QueuedSwarmChildrenAfter(ctx, 0)
}

// QueuedSwarmChildrenAfter scans a bounded window of FIFO allocation heads.
// A dispatcher rotates the private row-position cursor to avoid starvation by
// permanently stale heads. A cursor is neither identity nor execution authority.
func (s *Store) QueuedSwarmChildrenAfter(ctx context.Context, after int64) ([]SwarmChildWork, error) {
	if after < 0 {
		return nil, ErrSwarmChildConflict
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT c.id FROM swarm_child_tasks c WHERE c.rowid>? AND c.state='pending' AND c.delivery='queued'
 AND NOT EXISTS(SELECT 1 FROM swarm_child_tasks x WHERE x.actor_uid=c.actor_uid AND (x.state='running' OR (x.state='pending' AND x.delivery='queued' AND x.rowid<c.rowid))) ORDER BY c.rowid LIMIT 128`, after)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []SwarmChildWork{}
	for _, id := range ids {
		w, err := s.SwarmChildWorkByID(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, nil
}

// RecordSwarmChildDelivery records evidence after an attempt, independent of
// subsequently revoked authority. Only sending can settle, and never requeues.
// A completion received before the receipt is stronger evidence and is retained.
func (s *Store) RecordSwarmChildDelivery(ctx context.Context, p SwarmChildParent, key, delivery, detail string) error {
	if delivery != "delivered" && delivery != "not_sent" && delivery != "unconfirmed" {
		return ErrSwarmChildConflict
	}
	if plainTextProblem(detail, 1024) != "" {
		return ErrSwarmChildConflict
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		j, err := s.loadChildJoin(ctx, p)
		if err != nil {
			return err
		}
		for _, c := range j.Children {
			if c.RequestID != key {
				continue
			}
			if c.State == "completed" {
				return nil
			}
			if c.State != "running" || c.Delivery != "sending" {
				return ErrSwarmChildConflict
			}
			_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarm_child_tasks SET delivery=?,delivery_detail=?,revision=revision+1 WHERE id=? AND revision=? AND delivery='sending'`, delivery, detail, c.ID, c.Revision)
			return err
		}
		return ErrNotFound
	})
}

// RecoverSwarmChildDeliveries must run at exclusive server startup. Sending
// may already have caused effects; it is retained as uncertain without a resend.
func (s *Store) RecoverSwarmChildDeliveries(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_child_tasks SET delivery='unconfirmed',delivery_detail='The server restarted during delivery; the task may have reached the actor and is not resent.',revision=revision+1 WHERE state='running' AND delivery='sending'`)
	return err
}

// ChildJoinReport preserves every child's identity and outcome within the same
// report bound as actor steps. Each truncated report is marked explicitly;
// individual reports remain available unchanged from the child status API.
func ChildJoinReport(j SwarmChildJoin) string {
	header := fmt.Sprintf("Child join %s: %s; outstanding obligations: %d.\n", j.Policy.Mode, j.Evaluation.State, j.Evaluation.Open)
	headers := make([]string, len(j.Children)+len(j.UnregisteredFailures))
	texts := make([]string, len(headers))
	serverReason := make([]bool, len(headers))
	size := len(header)
	for i, c := range j.Children {
		outcome := c.Outcome
		if c.State == "cancelled" {
			outcome = "cancelled"
		}
		headers[i] = fmt.Sprintf("\nChild %s (%s):\n", c.ID, outcome)
		texts[i] = c.Report
		for _, f := range j.ExecutorFailures {
			if f.ChildID == c.ID {
				headers[i] = fmt.Sprintf("\nChild %s (server executor failure; not an actor report; job %s):\n", c.ID, f.IntentID)
				texts[i], serverReason[i] = f.Reason, true
			}
		}
		size += len(headers[i])
	}
	for n, f := range j.UnregisteredFailures {
		i := len(j.Children) + n
		headers[i] = fmt.Sprintf("\nJob %s (failed before registration; not a child report):\n", f.IntentID)
		texts[i] = f.Reason
		size += len(headers[i])
	}
	budget := MaxStepReportBytes - size
	if len(headers) > 0 {
		budget /= len(headers)
	}
	var b strings.Builder
	b.WriteString(header)
	for i, text := range texts {
		b.WriteString(headers[i])
		if len(text) > budget {
			marker := "\n[report truncated; read child status]\n"
			if i >= len(j.Children) || serverReason[i] {
				marker = "\n[reason truncated; read job history]\n"
			}
			n := max(0, budget-len(marker))
			for n > 0 && !utf8.ValidString(text[:n]) {
				n--
			}
			text = text[:n] + marker
		}
		b.WriteString(text)
	}
	return b.String()
}

type childJoinCompletionKey struct{}

// JoinSwarmChildren evaluates without blocking. Early any/quorum readiness is
// returned but never advances the graph while any real or absent child is open.
// Once settled, the server computes both the parent outcome and bounded report.
func (s *Store) JoinSwarmChildren(ctx context.Context, p SwarmChildParent, authorize SwarmChildAuthorize) (SwarmChildJoin, error) {
	var result SwarmChildJoin
	err := s.WithTx(ctx, func(ctx context.Context) error {
		j, err := s.GetSwarmChildJoin(ctx, p, authorize)
		if err != nil {
			return err
		}
		result = j
		if j.Evaluation.Open != 0 || j.Interrupted || j.State == "aborted" {
			return nil
		}
		usesPolicy, err := s.PipelineCompletionUsesPolicy(ctx, p.Principal.SwarmID, p.Sequence)
		if err != nil {
			return err
		}
		if usesPolicy {
			return ErrPipelineCompletionPending
		}
		outcome := "fail"
		if j.Evaluation.State == "ready_success" {
			outcome = "success"
		}
		return s.CompletePipelineActorVisit(context.WithValue(ctx, childJoinCompletionKey{}, true), p.Principal.SwarmID, p.Principal.SwarmID, p.Sequence, p.RequestID, p.Principal.ActorUID, p.Principal.Incarnation, outcome, ChildJoinReport(j))
	})
	if err != nil {
		return SwarmChildJoin{}, err
	}
	return result, nil
}

func (s *Store) checkChildParentResult(ctx context.Context, swarm string, sequence int64, outcome, report string) error {
	var engine bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarm_child_tasks WHERE swarm_id=? AND parent_sequence=? AND role!='') OR EXISTS(SELECT 1 FROM ephemeral_child_intents WHERE swarm_id=? AND parent_sequence=?)`, swarm, sequence, swarm, sequence).Scan(&engine); err != nil {
		return err
	}
	if !engine {
		return nil
	}
	p, err := s.SwarmChildParentAt(ctx, swarm, sequence)
	if err != nil {
		return err
	}
	j, err := s.loadChildJoin(ctx, p)
	if err != nil {
		return err
	}
	if j.State == "aborted" {
		return nil
	}
	if allowed, _ := ctx.Value(childJoinCompletionKey{}).(bool); !allowed {
		return ErrSwarmChildConflict
	}
	expected := "fail"
	if j.Evaluation.State == "ready_success" {
		expected = "success"
	}
	if j.Evaluation.Open != 0 || outcome != expected || report != ChildJoinReport(j) {
		return ErrSwarmChildConflict
	}
	return nil
}
