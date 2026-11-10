package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"brood.dev/go/brood/pipeline"
)

// ErrPipelineVisitConflict indicates that a visit can no longer be answered.
var ErrPipelineVisitConflict = errors.New("pipeline visit is no longer awaiting an answer")

// MaxPipelineGraphBytes bounds the persisted document before parsing.
const MaxPipelineGraphBytes = 1 << 20
const maxPipelineFieldBytes = 4096

// PipelineVisit identifies one visit to a node of the swarm graph.
// A visit belongs to its swarm directly: the run object is gone (#206).
type PipelineVisit struct {
	Sequence              int64
	NodeID, State         string
	StartedAt, FinishedAt *string
	Answer                *string
	Outcome               *string
	ActorUID              string
	ActorIncarnation      int64
	RequestID             string
	// ActorName is filled by reads when the stage actor is known: the managed
	// actor with ActorUID in the step's own swarm.
	ActorName string
	// Replay means the request was already reserved and must not be sent again.
	Replay bool
}

func validPipelineField(s string) bool { return len(s) > 0 && len(s) <= maxPipelineFieldBytes }

// activeExecution reports whether the swarm's graph is still running.
func activeExecution(execution string) bool {
	return execution == "running" || execution == "waiting"
}

// SwarmSteps returns the visits of the swarm graph in sequence order.
// Only the swarm's own steps are visible; an empty swarm has no steps.
func (s *Store) SwarmSteps(ctx context.Context, swarmID string) ([]PipelineVisit, error) {
	if !validPipelineField(swarmID) {
		return nil, errors.New("invalid swarm ID")
	}
	rows, err := s.conn(ctx).QueryContext(ctx, swarmStepSelect+` WHERE s.swarm_id=? ORDER BY s.sequence`, swarmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PipelineVisit{}
	for rows.Next() {
		v, err := scanSwarmStep(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// swarmStepSelect resolves managed names by UID within the step's own swarm,
// including deleted actors whose names may since have been reused (#206:
// join к запуску исчез — шаг принадлежит рою напрямую).
const swarmStepSelect = `SELECT s.sequence,s.node_id,s.state,s.started_at,s.finished_at,(SELECT q.outcome FROM swarm_requests q WHERE q.swarm_id=s.swarm_id AND q.sequence=s.sequence AND q.origin='node' AND q.state='answered'),s.outcome,COALESCE(s.actor_uid,''),COALESCE(s.actor_incarnation,0),COALESCE(s.request_id,''),COALESCE(m.name,'') FROM swarm_steps s LEFT JOIN managed_actors m ON m.uid=s.actor_uid AND m.swarm_id=s.swarm_id`

func scanSwarmStep(row interface{ Scan(...any) error }) (PipelineVisit, error) {
	var v PipelineVisit
	err := row.Scan(&v.Sequence, &v.NodeID, &v.State, &v.StartedAt, &v.FinishedAt, &v.Answer, &v.Outcome, &v.ActorUID, &v.ActorIncarnation, &v.RequestID, &v.ActorName)
	return v, err
}

// AppendPipelineVisit atomically allocates the next sequence within the
// swarm graph. The legacy run ID argument names the run's own swarm and
// must match: guest tooling still addresses visits by run ID (#210
// заменит провод). A new inflight actor visit names the actor bound to the
// node's role at its current incarnation; the binding is resolved in the
// same transaction as the write, and any other actor, incarnation or a
// stale, foreign, inactive or missing binding is refused with
// ErrPipelineVisitConflict before anything is written. A replay of an
// already recorded request ID writes nothing and is answered from the
// stored step without rechecking the binding.
func (s *Store) AppendPipelineVisit(ctx context.Context, swarmID, legacyRunID string, v PipelineVisit) (PipelineVisit, error) {
	if legacyRunID != swarmID {
		return v, ErrPipelineVisitConflict
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		v, err = s.appendPipelineVisit(ctx, swarmID, v)
		return err
	})
	return v, err
}

func (s *Store) appendPipelineVisit(ctx context.Context, swarmID string, v PipelineVisit) (PipelineVisit, error) {
	v.Replay = false
	if !validPipelineField(v.NodeID) || !validPipelineField(v.State) || len(v.ActorUID) > maxPipelineFieldBytes || len(v.RequestID) > maxPipelineFieldBytes || v.Answer != nil {
		return v, errors.New("invalid pipeline visit fields")
	}
	w, err := s.GetSwarm(ctx, swarmID)
	if err != nil {
		return v, err
	}
	if w.Graph == nil {
		return v, ErrPipelineVisitConflict
	}
	if w.Source != nil && v.State == "inflight" {
		return v, ErrPipelineSourceUnverified
	}
	if v.State == "inflight" && v.RequestID != "" {
		var node, actor, state string
		var sequence, incarnation int64
		err := s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,COALESCE(actor_uid,''),COALESCE(actor_incarnation,0),state FROM swarm_steps WHERE swarm_id=? AND request_id=?`, swarmID, v.RequestID).Scan(&sequence, &node, &actor, &incarnation, &state)
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
	if !activeExecution(w.Execution) {
		return v, errors.New("swarm graph is not active")
	}
	d, err := pipeline.Parse(string(w.Graph), "pipeline")
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
		// Узел называет роль (#208): шаг адресует только актора, привязанного
		// к ней, в нынешней инкарнации. Проверка в транзакции записи — обе
		// ветки ниже (первый шаг и ожидающий) пишут actor_uid после неё.
		if err := s.requireRoleActor(ctx, swarmID, n.Role, v.ActorUID, v.ActorIncarnation); err != nil {
			return v, fmt.Errorf("%w: role %q: %w", ErrPipelineVisitConflict, n.Role, err)
		}
	} else if v.State != "waiting" || n.Type != "human" || v.ActorUID != "" || v.ActorIncarnation != 0 || v.RequestID != "" {
		return v, errors.New("invalid pipeline visit state or metadata")
	}
	var count int
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*) FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&count); err != nil {
		return v, err
	}
	if count == 0 && v.NodeID != d.Start {
		return v, ErrPipelineVisitConflict
	}
	if count > 0 {
		if v.State == "inflight" {
			result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='inflight',started_at=CASE WHEN started_at IS NULL THEN ? ELSE started_at END,actor_uid=?,actor_incarnation=?,request_id=? WHERE swarm_id=? AND sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?) AND node_id=? AND state='pending'`, nowRFC3339(), v.ActorUID, v.ActorIncarnation, v.RequestID, swarmID, swarmID, v.NodeID)
			if err != nil {
				return v, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return v, err
			}
			if n == 1 {
				err = s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&v.Sequence)
				return v, err
			}
			var node, actor string
			var incarnation int64
			err = s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,actor_uid,actor_incarnation,state FROM swarm_steps WHERE swarm_id=? AND request_id=?`, swarmID, v.RequestID).Scan(&v.Sequence, &node, &actor, &incarnation, &v.State)
			if err == nil && node == v.NodeID && actor == v.ActorUID && incarnation == v.ActorIncarnation {
				v.Replay = true
				return v, nil
			}
		}
		return v, ErrPipelineVisitConflict
	}
	err = s.conn(ctx).QueryRowContext(ctx, `INSERT INTO swarm_steps (swarm_id,sequence,node_id,state,started_at,actor_uid,actor_incarnation,request_id) SELECT ?, (SELECT COALESCE(MAX(sequence),0)+1 FROM swarm_steps WHERE swarm_id=?), ?, ?, ?, NULLIF(?,''), ?, NULLIF(?,'') WHERE NOT EXISTS (SELECT 1 FROM swarm_steps WHERE swarm_id=? AND state IN ('inflight','unconfirmed')) ON CONFLICT(swarm_id,request_id) DO NOTHING RETURNING sequence`, swarmID, swarmID, v.NodeID, v.State, nowRFC3339(), v.ActorUID, v.ActorIncarnation, v.RequestID, swarmID).Scan(&v.Sequence)
	if errors.Is(err, sql.ErrNoRows) && v.RequestID != "" {
		var node, actor string
		var incarnation int64
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,actor_uid,actor_incarnation,state FROM swarm_steps WHERE swarm_id=? AND request_id=?`, swarmID, v.RequestID).Scan(&v.Sequence, &node, &actor, &incarnation, &v.State)
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
	if err == nil && v.State == "waiting" {
		err = s.openNodeRequest(ctx, swarmID, v.Sequence, n)
	}
	return v, err
}

// CompletePipelineActorVisit records an actor outcome or accepts an exact retry.
func (s *Store) CompletePipelineActorVisit(ctx context.Context, swarmID, legacyRunID string, sequence int64, requestID, actorUID string, incarnation int64, outcome string) error {
	if legacyRunID != swarmID || !validPipelineField(swarmID) || sequence < 1 || !validPipelineField(requestID) || !validPipelineField(actorUID) || incarnation < 1 || (outcome != "success" && outcome != "fail") {
		return errors.New("invalid pipeline actor completion")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		if w.Source != nil {
			return ErrPipelineSourceUnverified
		}
		var node, state, request, actor string
		var inc int64
		var prior sql.NullString
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state,COALESCE(request_id,''),COALESCE(actor_uid,''),COALESCE(actor_incarnation,0),outcome FROM swarm_steps WHERE swarm_id=? AND sequence=?`, swarmID, sequence).Scan(&node, &state, &request, &actor, &inc, &prior)
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
		if state != "inflight" || !activeExecution(w.Execution) {
			return ErrPipelineVisitConflict
		}
		d, err := pipeline.Parse(string(w.Graph), "pipeline")
		if err != nil {
			return err
		}
		return s.progressPipeline(ctx, w, d, sequence, node, "completed", outcome)
	})

}

// RecoverPipelineActions marks all inflight actor effects unconfirmed, including
// bound actors. It neither resends nor reconciles them; they remain fail-closed.
// Call only with exclusive ownership after the previous server has stopped.
// This is not wired into startup and does not provide automatic recovery.
func (s *Store) RecoverPipelineActions(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='unconfirmed' WHERE state='inflight'`)
	return err
}

// supportedPipeline rejects execution semantics this runner cannot honor.
func supportedPipeline(d *pipeline.Document) error { return pipeline.Supported(d) }

// progressPipeline applies one transition computed by the pure engine.
// The store owns only the sequence currency check and the write: the visit
// is finalized with its timestamps (started_at при переходе в работу, у
// завершённых шагов — finished_at), the successor visit and the swarm
// execution follow the transition. Всё выполняется в транзакции
// вызывающего, так что неудачная запись откатывается без частичного шага.
func (s *Store) progressPipeline(ctx context.Context, w Swarm, d *pipeline.Document, sequence int64, node, state, value string) error {
	var latest int64
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, w.ID).Scan(&latest); err != nil {
		return err
	}
	if latest != sequence {
		return ErrPipelineVisitConflict
	}
	transition, err := pipeline.Advance(d, pipeline.Event{Node: node, State: state, Outcome: value})
	if err != nil {
		return err
	}
	// Исход актёра и ответ оператора — разные поля: без подстановки имени
	// колонки строкой каждая ветка пишет своё поле явно. Ответ живёт
	// в запросе роя (#209), а не в шаге.
	if state == "answered" {
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state=?,finished_at=? WHERE swarm_id=? AND sequence=?`, state, nowRFC3339(), w.ID, sequence); err != nil {
			return err
		}
	} else if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state=?,outcome=?,finished_at=? WHERE swarm_id=? AND sequence=?`, state, value, nowRFC3339(), w.ID, sequence); err != nil {
		return err
	}
	if transition.Terminal {
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET execution=? WHERE id=?`, executionOfTransition(transition.Execution), w.ID)
		return err
	}
	if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES (?,?,?,?)`, w.ID, sequence+1, transition.Next, transition.NextState); err != nil {
		return err
	}
	if next := d.Nodes[transition.Next]; next.Type == "human" && transition.NextState == "waiting" {
		if err := s.openNodeRequest(ctx, w.ID, sequence+1, next); err != nil {
			return err
		}
	}
	_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET execution=? WHERE id=?`, executionOfTransition(transition.Execution), w.ID)
	return err
}

// executionOfTransition переводит статус прогона чистого движка (#207)
// в состояние исполнения роя (#206): waiting_human — это waiting,
// прочие значения совпадают.
func executionOfTransition(execution string) string {
	if execution == "waiting_human" {
		return "waiting"
	}
	return execution
}
