package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"brood.dev/go/brood/pipeline"
)

// ErrPipelineVisitConflict indicates that a visit can no longer be answered.
var ErrPipelineVisitConflict = errors.New("pipeline visit is no longer awaiting an answer")

// MaxPipelineGraphBytes bounds the persisted document before parsing.
const MaxPipelineGraphBytes = 1 << 20
const maxPipelineFieldBytes = 4096

// PipelineRun contains a persisted, immutable pipeline document.
type PipelineRun struct {
	ID, SwarmID, OwnerID string
	Graph                []byte
	Status               string
}

// PipelineVisit identifies one visit to a node within a run.
type PipelineVisit struct {
	Sequence         int64
	NodeID, State    string
	Answer           *string
	Outcome          *string
	ActorUID         string
	ActorIncarnation int64
	RequestID        string
	// Replay means the request was already reserved and must not be sent again.
	Replay bool
}

func validPipelineField(s string) bool { return len(s) > 0 && len(s) <= maxPipelineFieldBytes }

// CreatePipelineRun stores a validated graph snapshot. Authorization belongs to the caller.
func (s *Store) CreatePipelineRun(ctx context.Context, r PipelineRun) error {
	if !validPipelineField(r.ID) || !validPipelineField(r.SwarmID) || !validPipelineField(r.OwnerID) || !validPipelineField(r.Status) || len(r.Graph) == 0 || len(r.Graph) > MaxPipelineGraphBytes {
		return errors.New("invalid pipeline run fields or graph size")
	}
	d, err := pipeline.Parse(string(r.Graph), "pipeline")
	if err != nil {
		return err
	}
	if err = d.Validate(); err != nil {
		return err
	}
	if err = supportedPipeline(d); err != nil {
		return err
	}
	_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_runs (id,swarm_id,owner_id,graph,status) VALUES (?,?,?,?,?)`, r.ID, r.SwarmID, r.OwnerID, r.Graph, r.Status)
	return err
}

// GetPipelineRun retrieves a run only within the supplied swarm.
func (s *Store) GetPipelineRun(ctx context.Context, swarmID, id string) (PipelineRun, error) {
	var r PipelineRun
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT id,swarm_id,owner_id,graph,status FROM pipeline_runs WHERE swarm_id=? AND id=?`, swarmID, id).Scan(&r.ID, &r.SwarmID, &r.OwnerID, &r.Graph, &r.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ListPipelineRuns lists the runs of a swarm in ID order.
func (s *Store) ListPipelineRuns(ctx context.Context, swarmID string) ([]PipelineRun, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT id,swarm_id,owner_id,graph,status FROM pipeline_runs WHERE swarm_id=? ORDER BY id`, swarmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PipelineRun{}
	for rows.Next() {
		var r PipelineRun
		if err = rows.Scan(&r.ID, &r.SwarmID, &r.OwnerID, &r.Graph, &r.Status); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// AppendPipelineVisit atomically allocates the next sequence within a run.
func (s *Store) AppendPipelineVisit(ctx context.Context, swarmID, id string, v PipelineVisit) (PipelineVisit, error) {
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		v, err = s.appendPipelineVisit(ctx, swarmID, id, v)
		return err
	})
	return v, err
}

func (s *Store) appendPipelineVisit(ctx context.Context, swarmID, id string, v PipelineVisit) (PipelineVisit, error) {
	v.Replay = false
	if !validPipelineField(v.NodeID) || !validPipelineField(v.State) || len(v.ActorUID) > maxPipelineFieldBytes || len(v.RequestID) > maxPipelineFieldBytes || v.Answer != nil {
		return v, errors.New("invalid pipeline visit fields")
	}
	r, err := s.GetPipelineRun(ctx, swarmID, id)
	if err != nil {
		return v, err
	}
	if v.State == "inflight" && v.RequestID != "" {
		var node, actor, state string
		var sequence, incarnation int64
		err := s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,COALESCE(actor_uid,''),COALESCE(actor_incarnation,0),state FROM pipeline_steps WHERE run_id=? AND request_id=?`, id, v.RequestID).Scan(&sequence, &node, &actor, &incarnation, &state)
		if err == nil {
			if node != v.NodeID || actor != v.ActorUID || incarnation != v.ActorIncarnation {
				return v, errors.New("pipeline request ID reused with different payload")
			}
			v.Sequence, v.State, v.Replay = sequence, state, true
			return v, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return v, err
		}
	}
	if r.Status != "running" && r.Status != "waiting_human" {
		return v, errors.New("pipeline run is not active")
	}
	d, err := pipeline.Parse(string(r.Graph), "pipeline")
	if err != nil {
		return v, err
	}
	n, ok := d.Nodes[v.NodeID]
	if !ok {
		return v, errors.New("pipeline node not found")
	}
	if v.State == "inflight" {
		if n.Type != "actor" || !validPipelineField(v.ActorUID) || v.ActorIncarnation <= 0 || !validPipelineField(v.RequestID) {
			return v, errors.New("invalid inflight pipeline visit")
		}
	} else if v.State != "waiting" || n.Type != "human" || v.ActorUID != "" || v.ActorIncarnation != 0 || v.RequestID != "" {
		return v, errors.New("invalid pipeline visit state or metadata")
	}
	var count int
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*) FROM pipeline_steps WHERE run_id=?`, id).Scan(&count); err != nil {
		return v, err
	}
	if count == 0 && v.NodeID != d.Start {
		return v, ErrPipelineVisitConflict
	}
	if count > 0 {
		if v.State == "inflight" {
			result, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state='inflight',actor_uid=?,actor_incarnation=?,request_id=? WHERE run_id=? AND sequence=(SELECT MAX(sequence) FROM pipeline_steps WHERE run_id=?) AND node_id=? AND state='pending'`, v.ActorUID, v.ActorIncarnation, v.RequestID, id, id, v.NodeID)
			if err != nil {
				return v, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return v, err
			}
			if n == 1 {
				err = s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM pipeline_steps WHERE run_id=?`, id).Scan(&v.Sequence)
				return v, err
			}
			var node, actor string
			var incarnation int64
			err = s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,actor_uid,actor_incarnation,state FROM pipeline_steps WHERE run_id=? AND request_id=?`, id, v.RequestID).Scan(&v.Sequence, &node, &actor, &incarnation, &v.State)
			if err == nil && node == v.NodeID && actor == v.ActorUID && incarnation == v.ActorIncarnation {
				v.Replay = true
				return v, nil
			}
		}
		return v, ErrPipelineVisitConflict
	}
	err = s.conn(ctx).QueryRowContext(ctx, `INSERT INTO pipeline_steps (run_id,sequence,node_id,state,actor_uid,actor_incarnation,request_id) SELECT id, (SELECT COALESCE(MAX(sequence),0)+1 FROM pipeline_steps WHERE run_id=pipeline_runs.id), ?, ?, NULLIF(?,''), ?, NULLIF(?,'') FROM pipeline_runs WHERE swarm_id=? AND id=? AND NOT EXISTS (SELECT 1 FROM pipeline_steps WHERE run_id=pipeline_runs.id AND state IN ('inflight','unconfirmed')) ON CONFLICT(run_id,request_id) DO NOTHING RETURNING sequence`, v.NodeID, v.State, v.ActorUID, v.ActorIncarnation, v.RequestID, swarmID, id).Scan(&v.Sequence)
	if errors.Is(err, sql.ErrNoRows) && v.RequestID != "" {
		var node, actor string
		var incarnation int64
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT s.sequence,s.node_id,s.actor_uid,s.actor_incarnation,s.state FROM pipeline_steps s JOIN pipeline_runs r ON r.id=s.run_id WHERE r.swarm_id=? AND r.id=? AND s.request_id=?`, swarmID, id, v.RequestID).Scan(&v.Sequence, &node, &actor, &incarnation, &v.State)
		if err == nil {
			if node != v.NodeID || actor != v.ActorUID || incarnation != v.ActorIncarnation {
				return v, errors.New("pipeline request ID reused with different payload")
			}
			v.Replay = true
			return v, nil
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrPipelineVisitConflict
	}
	return v, err
}

// ListPipelineVisits returns visits in sequence order, scoped by swarm.
func (s *Store) ListPipelineVisits(ctx context.Context, swarmID, id string) ([]PipelineVisit, error) {
	if _, err := s.GetPipelineRun(ctx, swarmID, id); err != nil {
		return nil, err
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT s.sequence,s.node_id,s.state,s.answer,s.outcome,COALESCE(s.actor_uid,''),COALESCE(s.actor_incarnation,0),COALESCE(s.request_id,'') FROM pipeline_steps s JOIN pipeline_runs r ON r.id=s.run_id WHERE r.swarm_id=? AND r.id=? ORDER BY s.sequence`, swarmID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PipelineVisit{}
	for rows.Next() {
		var v PipelineVisit
		if err = rows.Scan(&v.Sequence, &v.NodeID, &v.State, &v.Answer, &v.Outcome, &v.ActorUID, &v.ActorIncarnation, &v.RequestID); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// AnswerPipelineVisit accepts one answer only for the latest waiting human visit.
func (s *Store) AnswerPipelineVisit(ctx context.Context, swarmID, id string, sequence int64, answer string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		return s.answerPipelineVisit(ctx, swarmID, id, sequence, answer)
	})
}

func (s *Store) answerPipelineVisit(ctx context.Context, swarmID, id string, sequence int64, answer string) error {
	if !validPipelineField(answer) || sequence < 1 {
		return errors.New("invalid pipeline answer")
	}
	r, err := s.GetPipelineRun(ctx, swarmID, id)
	if err != nil {
		return err
	}
	var node, state string
	var previous sql.NullString
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state,answer FROM pipeline_steps WHERE run_id=? AND sequence=?`, id, sequence).Scan(&node, &state, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "answered" && previous.Valid && previous.String == answer {
		return nil
	}
	if state != "waiting" || (r.Status != "running" && r.Status != "waiting_human") {
		return ErrPipelineVisitConflict
	}
	d, err := pipeline.Parse(string(r.Graph), "pipeline")
	if err != nil {
		return err
	}
	n, ok := d.Nodes[node]
	if !ok || n.Type != "human" {
		return errors.New("visit is not a human node")
	}
	allowed := false
	for _, option := range n.Options {
		if option.Outcome == answer {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("answer is not a human option")
	}
	return s.progressPipeline(ctx, r, d, sequence, node, "answered", answer)

}

// CompletePipelineActorVisit records an actor outcome or accepts an exact retry.
func (s *Store) CompletePipelineActorVisit(ctx context.Context, swarmID, id string, sequence int64, requestID, actorUID string, incarnation int64, outcome string) error {
	if !validPipelineField(swarmID) || !validPipelineField(id) || sequence < 1 || !validPipelineField(requestID) || !validPipelineField(actorUID) || incarnation < 1 || (outcome != "success" && outcome != "fail") {
		return errors.New("invalid pipeline actor completion")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		r, err := s.GetPipelineRun(ctx, swarmID, id)
		if err != nil {
			return err
		}
		var node, state, request, actor string
		var inc int64
		var prior sql.NullString
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state,COALESCE(request_id,''),COALESCE(actor_uid,''),COALESCE(actor_incarnation,0),outcome FROM pipeline_steps WHERE run_id=? AND sequence=?`, id, sequence).Scan(&node, &state, &request, &actor, &inc, &prior)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPipelineVisitConflict
		}
		if err != nil {
			return err
		}
		if request != requestID || actor != actorUID || inc != incarnation {
			return ErrPipelineVisitConflict
		}
		if state == "completed" && prior.Valid && prior.String == outcome {
			return nil
		}
		if state != "inflight" || (r.Status != "running" && r.Status != "waiting_human") {
			return ErrPipelineVisitConflict
		}
		d, err := pipeline.Parse(string(r.Graph), "pipeline")
		if err != nil {
			return err
		}
		return s.progressPipeline(ctx, r, d, sequence, node, "completed", outcome)
	})

}

// RecoverPipelineActions marks all inflight actor effects unconfirmed, including
// bound actors. It neither resends nor reconciles them; they remain fail-closed.
// Call only with exclusive ownership after the previous server has stopped.
// This is not wired into startup and does not provide automatic recovery.
func (s *Store) RecoverPipelineActions(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state='unconfirmed' WHERE state='inflight'`)
	return err
}

// supportedPipeline rejects execution semantics this runner cannot honor.
func supportedPipeline(d *pipeline.Document) error {
	if d.Retry != nil && d.Retry.MaxRetries != 0 {
		return errors.New("pipeline retries are unsupported")
	}
	for _, gate := range d.GoalGates {
		if gate != d.Exit {
			return errors.New("pipeline goal gates are unsupported")
		}
	}
	for id, n := range d.Nodes {
		if n.Type != "actor" && n.Type != "human" && n.Type != "exit" {
			return errors.New("pipeline node type is unsupported")
		}
		if n.Type == "exit" && id != d.Exit {
			return errors.New("pipeline has a non-designated exit")
		}
		if n.Type == "human" {
			seen := map[string]bool{}
			for _, option := range n.Options {
				if seen[option.Outcome] {
					return errors.New("ambiguous pipeline human outcome")
				}
				seen[option.Outcome] = true
				if _, ok := selectPipelineEdge(d, id, "answered", option.Outcome); !ok {
					return errors.New("pipeline human option has no matching edge")
				}
			}
		}
	}
	for _, e := range d.Edges {
		if e.Condition != "" {
			for _, clause := range strings.Split(e.Condition, "&&") {
				if !strings.HasPrefix(strings.TrimSpace(clause), "outcome=") && !strings.HasPrefix(strings.TrimSpace(clause), "outcome!=") {
					return errors.New("pipeline condition is unsupported")
				}
			}
		}
	}
	return nil
}

func selectPipelineEdge(d *pipeline.Document, node, state, value string) (pipeline.Edge, bool) {
	preferred := ""
	if state == "answered" {
		for _, option := range d.Nodes[node].Options {
			if option.Outcome == value {
				preferred = option.Label
				break
			}
		}
	}
	var edges []pipeline.Edge
	for _, edge := range d.Edges {
		if edge.From != node || (state == "completed" && value == "fail" && edge.Condition == "") {
			continue
		}
		if state == "answered" && edge.Label != "" && edge.Label != preferred {
			continue
		}
		edges = append(edges, edge)
	}
	return pipeline.SelectNext(edges, value, nil, preferred, nil)
}

func (s *Store) progressPipeline(ctx context.Context, r PipelineRun, d *pipeline.Document, sequence int64, node, state, value string) error {
	if err := supportedPipeline(d); err != nil {
		return err
	}
	var latest int64
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM pipeline_steps WHERE run_id=?`, r.ID).Scan(&latest); err != nil {
		return err
	}
	if latest != sequence {
		return ErrPipelineVisitConflict
	}
	edge, ok := selectPipelineEdge(d, node, state, value)
	if !ok {
		column := "outcome"
		if state == "answered" {
			column = "answer"
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state=?, `+column+`=? WHERE run_id=? AND sequence=?`, state, value, r.ID, sequence); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_runs SET status='failed' WHERE id=?`, r.ID)
		return err
	}
	if d.Nodes[edge.To].Type == "exit" && edge.To != d.Exit {
		return errors.New("pipeline has a non-designated exit")
	}
	next := d.Nodes[edge.To]
	nextState := "pending"
	status := "running"
	switch next.Type {
	case "human":
		nextState = "waiting"
		status = "waiting_human"
	case "exit":
		nextState = "completed"
		status = "succeeded"
		if value == "fail" {
			status = "failed"
		}
	case "actor":
	default:
		return errors.New("pipeline node type is unsupported")
	}
	column := "outcome"
	if state == "answered" {
		column = "answer"
	}
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state=?, `+column+`=? WHERE run_id=? AND sequence=?`, state, value, r.ID, sequence); err != nil {
		return err
	}
	if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_steps(run_id,sequence,node_id,state) VALUES (?,?,?,?)`, r.ID, sequence+1, edge.To, nextState); err != nil {
		return err
	}
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_runs SET status=? WHERE id=?`, status, r.ID)
	return err
}
