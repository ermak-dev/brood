package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"brood.dev/go/brood/pipeline"
)

// ErrPipelineVisitConflict indicates that a visit can no longer be answered.
var ErrPipelineVisitConflict = errors.New("pipeline visit is no longer awaiting an answer")

// ErrStepReportInvalid: the actor's step report is not plain bounded text.
var ErrStepReportInvalid = errors.New("invalid step report")

// MaxStepReportBytes bounds the actor's step report.
const MaxStepReportBytes = 16384

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
	// AttestedCommit is the source commit the bound actor's guest attested
	// for this attempt (#183, #212); empty until the attestation is recorded.
	AttestedCommit string
	// Detail is the operator-facing reason of a refused or unconfirmed step.
	Detail string
	// Report is the actor's account of a completed step (#233).
	Report string
	// Goal is the goal of the pass this step opened (#242): only the first
	// step of a pass has one.
	Goal string
	// SourceCommit is the source commit of the pass this step opened (#229):
	// only the first step of a pass has one, together with Goal.
	SourceCommit string
	// Pass is the sequence of the first step of the step's pass; 0 for a
	// step outside a pass opened by StartSwarm.
	Pass int64
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
// Only the swarm's own steps are visible; an empty swarm has no steps. Each
// visit names the first step of its pass (Pass), which carries the pass goal.
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
	// Шаги идут по номеру: проход шага — последний открытый до него проход.
	var pass int64
	for rows.Next() {
		v, err := scanSwarmStep(rows)
		if err != nil {
			return nil, err
		}
		if v.Goal != "" {
			pass = v.Sequence
		}
		v.Pass = pass
		result = append(result, v)
	}
	return result, rows.Err()
}

// stepPass — первый шаг, задача и commit прохода, которому принадлежит шаг
// sequence: последний шаг роя с задачей не дальше него, как в SwarmSteps и
// в триггерах swarm_steps_attested. Шаг вне прохода, открытого запуском, —
// 0 и пустые задача и commit.
func (s *Store) stepPass(ctx context.Context, swarmID string, sequence int64) (pass int64, goal, commit string, err error) {
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,goal,source_commit FROM swarm_steps WHERE swarm_id=? AND sequence<=? AND goal IS NOT NULL ORDER BY sequence DESC LIMIT 1`, swarmID, sequence).Scan(&pass, &goal, &commit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", nil
	}
	return pass, goal, commit, err
}

// swarmStepSelect resolves managed names by UID within the step's own swarm,
// including deleted actors whose names may since have been reused (#206:
// join к запуску исчез — шаг принадлежит рою напрямую).
const swarmStepSelect = `SELECT s.sequence,s.node_id,s.state,s.started_at,s.finished_at,(SELECT q.outcome FROM swarm_requests q WHERE q.swarm_id=s.swarm_id AND q.sequence=s.sequence AND q.origin='node' AND q.state='answered'),s.outcome,COALESCE(s.actor_uid,''),COALESCE(s.actor_incarnation,0),COALESCE(s.request_id,''),COALESCE(s.attested_commit,''),COALESCE(s.detail,''),COALESCE(s.report,''),COALESCE(s.goal,''),COALESCE(s.source_commit,''),COALESCE(m.name,'') FROM swarm_steps s LEFT JOIN managed_actors m ON m.uid=s.actor_uid AND m.swarm_id=s.swarm_id`

func scanSwarmStep(row interface{ Scan(...any) error }) (PipelineVisit, error) {
	var v PipelineVisit
	err := row.Scan(&v.Sequence, &v.NodeID, &v.State, &v.StartedAt, &v.FinishedAt, &v.Answer, &v.Outcome, &v.ActorUID, &v.ActorIncarnation, &v.RequestID, &v.AttestedCommit, &v.Detail, &v.Report, &v.Goal, &v.SourceCommit, &v.ActorName)
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
//
// No production path writes inflight here: actor steps of a started swarm
// are delivered by ReserveActorStep and its attestation (#212), and an
// inflight visit of a swarm with a source snapshot is refused with
// ErrPipelineSourceUnverified because this path attests nothing.
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
		err = s.openNodeRequest(ctx, swarmID, v.Sequence, n, "")
	}
	return v, err
}

// CompletePipelineActorVisit records an actor outcome or accepts an exact retry.
// The caller is the actor recorded in the step: the request ID, UID and
// incarnation must match it, and the actor must still be the one bound to the
// node's role at that incarnation (#208), checked in the same transaction. A
// step of a swarm with a source snapshot takes an outcome only after its
// workspace attestation is recorded (attested_commit, #212); otherwise
// ErrPipelineSourceUnverified. The step must be the latest one of a running
// graph and be delivering (the actor answered before the server recorded the
// delivery), inflight or unconfirmed with a recorded attestation (the receipt
// was lost, but the request ID reaches the actor only in the instruction,
// #245); another unconfirmed, a refused, stopped or finished step is
// ErrPipelineVisitConflict.
//
// The optional report (#233) is the actor's account of the step: what was
// done and where the result is. It is written once, in the same update as the
// outcome, and a human node entered right after the step gets it as the
// material of its node request. A report that is not valid UTF-8, has control
// characters other than newline and tab or exceeds MaxStepReportBytes is
// ErrStepReportInvalid before anything is read. An exact retry, the recorded
// outcome with the recorded report, writes nothing and succeeds; any other
// outcome or report for the completed step is ErrPipelineVisitConflict.
func (s *Store) CompletePipelineActorVisit(ctx context.Context, swarmID, legacyRunID string, sequence int64, requestID, actorUID string, incarnation int64, outcome, report string) error {
	if legacyRunID != swarmID || !validPipelineField(swarmID) || sequence < 1 || !validPipelineField(requestID) || !validPipelineField(actorUID) || incarnation < 1 || (outcome != "success" && outcome != "fail") {
		return errors.New("invalid pipeline actor completion")
	}
	if err := validStepReport(report); err != nil {
		return err
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		var node, state, request, actor, attested, recorded string
		var inc int64
		var prior sql.NullString
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state,COALESCE(request_id,''),COALESCE(actor_uid,''),COALESCE(actor_incarnation,0),outcome,COALESCE(attested_commit,''),COALESCE(report,'') FROM swarm_steps WHERE swarm_id=? AND sequence=?`, swarmID, sequence).Scan(&node, &state, &request, &actor, &inc, &prior, &attested, &recorded)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPipelineVisitConflict
		}
		if err != nil {
			return err
		}
		if request != requestID || actor != actorUID || inc != incarnation {
			return ErrPipelineVisitConflict
		}
		if w.Source != nil {
			// Commit прохода шага, а не роя (#229): поздний исход шага
			// прошлого прохода сверяется со своим commit.
			_, _, commit, err := s.stepPass(ctx, swarmID, sequence)
			if err != nil {
				return err
			}
			if commit == "" || attested != commit {
				return ErrPipelineSourceUnverified
			}
		}
		if state == "completed" && prior.Valid && prior.String == outcome && recorded == report {
			return nil
		}
		// unconfirmed с подтверждённым checkout — шаг, чья квитанция доставки
		// потерялась: request_id попытки есть только в тексте инструкции, так
		// что вызов с ним доказывает её получение (#245). Без подтверждения
		// инструкция не уходила; unconfirmed старого пути без источника
		// по-прежнему ждёт решения оператора.
		lostReceipt := state == "unconfirmed" && attested != ""
		if (state != "inflight" && state != "delivering" && !lostReceipt) || !activeExecution(w.Execution) {
			return ErrPipelineVisitConflict
		}
		d, err := pipeline.Parse(string(w.Graph), "pipeline")
		if err != nil {
			return err
		}
		n, ok := d.Nodes[node]
		if !ok || n.Type != "actor" {
			return ErrPipelineVisitConflict
		}
		if err := s.requireRoleActor(ctx, swarmID, n.Role, actorUID, incarnation); err != nil {
			return fmt.Errorf("%w: role %q: %w", ErrPipelineVisitConflict, n.Role, err)
		}
		return s.progressPipeline(ctx, w, d, sequence, node, "completed", outcome, report)
	})

}

// RecoverPipelineActions marks all inflight actor effects unconfirmed, including
// bound actors. It neither resends nor reconciles them; they remain fail-closed.
// Call only with exclusive ownership after the previous server has stopped.
// It belongs to the reservation path of AppendPipelineVisit, where inflight
// does not tell a delivered instruction from one being sent; startup runs
// RecoverActorSteps instead, which keeps delivered steps inflight (#212).
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
// report — отчёт актора завершённого шага (#233), у ответа оператора пуст:
// он пишется тем же UPDATE, что исход, и становится материалом узлового
// запроса, если преемник — human-узел. Преемник — следующий шаг того же
// прохода, поэтому материал никогда не приходит из прошлого прохода.
func (s *Store) progressPipeline(ctx context.Context, w Swarm, d *pipeline.Document, sequence int64, node, state, value, report string) error {
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
	now := nowRFC3339()
	if state == "answered" {
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state=?,finished_at=? WHERE swarm_id=? AND sequence=?`, state, now, w.ID, sequence); err != nil {
			return err
		}
	} else if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state=?,outcome=?,finished_at=?,report=NULLIF(?,'') WHERE swarm_id=? AND sequence=?`, state, value, now, report, w.ID, sequence); err != nil {
		return err
	}
	if transition.Terminal {
		return s.advanceExecution(ctx, w.ID, executionOfTransition(transition.Execution), now)
	}
	if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES (?,?,?,?)`, w.ID, sequence+1, transition.Next, transition.NextState); err != nil {
		return err
	}
	if next := d.Nodes[transition.Next]; next.Type == "human" && transition.NextState == "waiting" {
		if err := s.openNodeRequest(ctx, w.ID, sequence+1, next, stepReportContext(sequence, node, value, report)); err != nil {
			return err
		}
	}
	return s.advanceExecution(ctx, w.ID, executionOfTransition(transition.Execution), now)
}

// advanceExecution пишет исполнение роя после перехода. Переход, кончающий
// проход (succeeded или failed: терминальный отказ или шаг выхода), тем же
// UPDATE пишет момент окончания в execution_at (#268): finished_at шага,
// исход которого кончил проход. Переход running ↔ waiting внутри прохода
// момент не трогает: он остаётся началом прохода.
func (s *Store) advanceExecution(ctx context.Context, swarmID, execution, finishedAt string) error {
	if activeExecution(execution) {
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET execution=? WHERE id=?`, execution, swarmID)
		return err
	}
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET execution=?,execution_at=? WHERE id=?`, execution, finishedAt, swarmID)
	return err
}

// stepReportContext — материал узлового запроса из отчёта шага, который
// ввёл граф в human-узел: номер шага, узел и исход в первой строке, затем
// отчёт как есть. Шаг без отчёта материала не даёт.
func stepReportContext(sequence int64, node, outcome, report string) string {
	if report == "" {
		return ""
	}
	return fmt.Sprintf("Report of step %d (node %s, outcome %s):\n\n%s", sequence, node, outcome, report)
}

// validStepReport: отчёт — валидный UTF-8 не длиннее MaxStepReportBytes без
// управляющих символов, кроме перевода строки и табуляции. Пустой отчёт —
// его отсутствие.
func validStepReport(report string) error {
	if problem := plainTextProblem(report, MaxStepReportBytes); problem != "" {
		return fmt.Errorf("%w: the report %s", ErrStepReportInvalid, problem)
	}
	return nil
}

// plainTextProblem называет, чем текст от актора или оператора (отчёт шага,
// задача прохода) не годится: длиннее limit байт, не UTF-8 или с
// управляющими символами, кроме перевода строки и табуляции. Годный текст —
// пустая строка в ответе; пустой текст годен, решает вызывающий.
func plainTextProblem(text string, limit int) string {
	switch {
	case len(text) > limit:
		return fmt.Sprintf("exceeds %d bytes", limit)
	case !utf8.ValidString(text):
		return "is not valid UTF-8"
	case strings.IndexFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0:
		return "has control characters other than newline and tab"
	}
	return ""
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
