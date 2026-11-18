package store

// FEATURE: Доставка инструкции узла привязанному актору (#212), хранилище.
// SPEC: Шаг actor-узла роя со снимком источника идёт pending → attesting →
// delivering → inflight. Резерв попытки — привязанный к роли актор, его
// инкарнация, request_id и workspace-lease подтверждения — пишется одной
// транзакцией до любого эффекта у гостя. Запись подтверждения workspace
// (#183) заново проверяет исполнение, привязку роли и источник и только она
// переводит шаг в delivering с attested_commit: это и снимает
// ErrPipelineSourceUnverified для шага. Исход доставки пишется из
// delivering: подтверждена — inflight, неизвестна — unconfirmed. Отказ,
// после которого инструкция заведомо не ушла, — refused с причиной.
// Попытка, до гостя не дошедшая (выключение сервера), возвращается в
// pending вместе с освобождением lease: WithdrawActorStep.
// IMPLEMENTATION: каждый переход — условный UPDATE по состоянию и
// request_id: повтор той же попытки и опоздавшая запись после остановки
// ничего не пишут. Сервер работает одним экземпляром и сериализует
// диспетчер в процессе, но корректность держат эти условия, а не процесс.
// Гостевые эффекты (подтверждение, сообщение) делает сервер вне транзакций.
// RESULT: IMPLEMENTED — схема 22: словарь состояний, attested_commit,
// detail, триггеры swarm_steps_attested.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"brood.dev/go/brood/pipeline"
)

// ErrNoActorStep: у роя нет шага actor-узла, ждущего доставки: рой не идёт,
// последний шаг не pending или у снимка нет источника. Ничего не записано.
var ErrNoActorStep = errors.New("swarm has no actor step awaiting delivery")

// ErrActorStepRefused: шаг закрыт как refused, причина записана в шаг.
var ErrActorStepRefused = errors.New("actor step was refused")

// WorkspaceAttestOperation — имя workspace-lease подтверждения в
// coordination_claims: подтверждение останавливает и перезапускает актора,
// как restart, и исключает те же операции.
const WorkspaceAttestOperation = "workspace_attest"

// maxStepDetailBytes — предел причины шага; тот же предел держит схема.
const maxStepDetailBytes = 1024

const (
	detailStoppedDelivery    = "the swarm stopped during instruction delivery; the delivery outcome is unknown"
	detailRestartAttestation = "the server restarted during workspace attestation; its outcome is unknown and no instruction was sent"
	detailRestartDelivery    = "the server restarted during instruction delivery; the delivery outcome is unknown"
)

// ActorStepAttempt — зарезервированная попытка доставки шага actor-узла.
type ActorStepAttempt struct {
	SwarmID, SwarmName string
	Sequence           int64
	NodeID             string
	// RequestID — личность попытки: request_id шага, step подтверждения и
	// ID workspace-lease.
	RequestID string
	// Actor — привязанный к роли узла актор на момент резерва.
	Actor ManagedActor
	// Source — origin роя и commit прохода этого шага (#229).
	Source PipelineSource
	Graph  *pipeline.Document
	// Pass and Goal — первый шаг прохода этого шага и задача прохода (#242),
	// прочитанные той же транзакцией резерва.
	Pass int64
	Goal string
	// Previous — итог предыдущего шага того же прохода (#248), прочитанный
	// той же транзакцией резерва; nil у первого шага прохода.
	Previous *PreviousStep
	// Lease — workspace-lease подтверждения; его исход пишет сервер
	// (FinishActorOperation) после ответа гостя.
	Lease ActorOperation
}

// PreviousStep — шаг прохода, непосредственно предшествующий шагу актора
// (#248): исход и отчёт actor-шага или отвеченный узловой запрос human-шага.
// Это текст для актора, а не полномочие: ни отчёт, ни ответ оператора прав
// не выдают.
type PreviousStep struct {
	Sequence int64
	NodeID   string
	// Outcome — исход шага; у human-шага — исход варианта, выбранного
	// оператором.
	Outcome string
	// Report — отчёт actor-шага (#233); пуст у human-шага и у шага без
	// отчёта.
	Report string
	// Request — отвеченный узловой запрос human-шага: вопрос, материал,
	// варианты и ответ оператора; nil у actor-шага.
	Request *SwarmRequest
}

// ReserveActorStep reserves the delivery attempt of the swarm's latest step
// when it is a pending actor step of a running swarm with a source snapshot.
// In one transaction the node's role is resolved to the bound actor at its
// current incarnation, the actor's workspace is reserved for the attestation
// (AcquireActorOperation, operation workspace_attest, ID requestID) and the
// step becomes attesting with that actor and request ID. Nothing to reserve
// is ErrNoActorStep without a write. A step whose pass has no source commit,
// a role that does not resolve to a live bound actor or a busy workspace
// closes the step as refused with the reason in the same transaction and
// answers ErrActorStepRefused. The attempt carries the source commit (#229)
// and the goal of the step's pass, the sequence of its first step and, read
// in the same transaction, the step right before it in that pass (Previous,
// #248): nil for the first step of a pass. A nonempty policy requires an exact
// durable candidate before any workspace lease. Initial and post-human entries
// retain their own candidate; immediate actor successors reuse their result
// candidate. Unsatisfied obligations return ErrPipelinePolicyPending without an
// actor attempt. Explicit denial/negative confirmation fails the pass with a
// readable reason and no actor outcome. A graph answer is never policy approval.
func (s *Store) ReserveActorStep(ctx context.Context, swarmID, requestID, caller string) (ActorStepAttempt, error) {
	if !validPipelineField(swarmID) || !validPipelineField(requestID) || caller == "" {
		return ActorStepAttempt{}, errors.New("invalid actor step reservation")
	}
	var attempt ActorStepAttempt
	var refusal string
	var policyHeld bool
	err := s.WithTx(ctx, func(ctx context.Context) error {
		w, sequence, node, d, err := s.awaitingActorStep(ctx, swarmID)
		if err != nil {
			return err
		}
		pass, goal, commit, err := s.stepPass(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		// Шаг без commit прохода (только мимо запуска) подтвердить нечем.
		if commit == "" {
			refusal = "the step's pass has no source commit to attest"
			return s.refusePendingStep(ctx, swarmID, sequence, refusal)
		}
		usesPolicy, err := s.PipelineCompletionUsesPolicy(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if usesPolicy {
			preview, boundary, err := s.actorEntryPolicyPreview(ctx, w, d, sequence)
			if errors.Is(err, errPolicyReviewUnavailable) {
				refusal = err.Error()
				return s.failPolicyPendingActor(ctx, swarmID, sequence, pass, node, refusal)
			}
			if err != nil {
				return err
			}
			if boundary == "entry" {
				if _, err := s.RecordPipelinePolicyCandidate(ctx, swarmID, sequence, "entry", NewID()); err != nil {
					return err
				}
			}
			candidate, err := s.GetPipelinePolicyCandidate(ctx, swarmID, preview.Sequence, boundary)
			if err != nil {
				return err
			}
			decision, rejected, err := s.pipelinePolicyCandidateDecision(ctx, candidate)
			if err != nil {
				return err
			}
			if rejected {
				refusal = policyRejectionReason(decision)
				return s.failPolicyPendingActor(ctx, swarmID, sequence, pass, node, refusal)
			}
			allowed, err := s.actorEntryPolicyAllowed(ctx, w, d, sequence)
			if err != nil {
				return err
			}
			if !allowed {
				policyHeld = true
				return nil
			}
			if err := s.assignInlinePolicyReview(ctx, candidate, sequence); err != nil {
				return err
			}
		}
		source, err := s.PipelineActorStepSource(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		commit = source.Commit
		n := d.Nodes[node]
		b, err := s.stepRoleBinding(ctx, swarmID, n.Role)
		if err != nil {
			refusal = fmt.Sprintf("role %q has no live bound actor: %v", n.Role, err)
			return s.refusePendingStep(ctx, swarmID, sequence, refusal)
		}
		actor, err := s.GetManagedActor(ctx, b.ActorUID)
		if err != nil {
			return err
		}
		lease, err := s.AcquireActorOperation(ctx, actor.UID, actor.Incarnation, requestID, caller, WorkspaceAttestOperation)
		if errors.Is(err, ErrCoordinationConflict) {
			refusal = fmt.Sprintf("actor %s cannot attest its workspace: %v", actor.Name, err)
			return s.refusePendingStep(ctx, swarmID, sequence, refusal)
		}
		if err != nil {
			return err
		}
		if err := s.markStepAttesting(ctx, swarmID, sequence, actor, requestID); err != nil {
			return err
		}
		previous, err := s.previousStep(ctx, d, swarmID, pass, sequence)
		if err != nil {
			return err
		}
		attempt = ActorStepAttempt{SwarmID: w.ID, SwarmName: w.Name, Sequence: sequence, NodeID: node, RequestID: requestID, Actor: actor, Source: PipelineSource{Origin: w.Source.Origin, Commit: commit}, Graph: d, Pass: pass, Goal: goal, Previous: previous, Lease: lease}
		return nil
	})
	if err != nil {
		return ActorStepAttempt{}, err
	}
	if refusal != "" {
		return ActorStepAttempt{}, fmt.Errorf("%w: %s", ErrActorStepRefused, refusal)
	}
	if policyHeld {
		return ActorStepAttempt{}, ErrPipelinePolicyPending
	}
	return attempt, nil
}

// previousStep читает шаг, предшествующий sequence в проходе pass (#248). У
// первого шага прохода (и у шага вне прохода) его нет: шаги прошлых
// проходов не читаются. Следующий шаг граф открывает только после
// завершённого actor-шага или отвеченного human-шага (progressPipeline),
// поэтому иное состояние — ошибка, и резерв откатывается.
func (s *Store) previousStep(ctx context.Context, d *pipeline.Document, swarmID string, pass, sequence int64) (*PreviousStep, error) {
	if pass < 1 || sequence <= pass {
		return nil, nil
	}
	p := PreviousStep{Sequence: sequence - 1}
	var state string
	var outcome sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state,outcome,COALESCE(report,'') FROM swarm_steps WHERE swarm_id=? AND sequence=?`, swarmID, p.Sequence).Scan(&p.NodeID, &state, &outcome, &p.Report)
	if err != nil {
		return nil, fmt.Errorf("previous step %d: %w", p.Sequence, err)
	}
	switch kind := d.Nodes[p.NodeID].Type; {
	case kind == "actor" && state == "completed" && outcome.Valid:
		p.Outcome = outcome.String
		return &p, nil
	case kind == "human" && state == "answered":
		req, err := scanSwarmRequest(s.conn(ctx).QueryRowContext(ctx, `SELECT `+swarmRequestCols+` FROM swarm_requests WHERE swarm_id=? AND sequence=? AND origin='node' AND state='answered'`, swarmID, p.Sequence))
		if err != nil {
			return nil, fmt.Errorf("node request of previous step %d: %w", p.Sequence, err)
		}
		if req.Outcome == nil {
			return nil, fmt.Errorf("node request of previous step %d has no outcome", p.Sequence)
		}
		p.Outcome, p.Request = *req.Outcome, &req
		return &p, nil
	}
	return nil, fmt.Errorf("previous step %d is %s at %s node %s", p.Sequence, state, d.Nodes[p.NodeID].Type, p.NodeID)
}

// awaitingActorStep находит последний шаг роя, ждущий доставки: идущий
// активный рой со снимком графа и источника, шаг pending actor-узла.
func (s *Store) awaitingActorStep(ctx context.Context, swarmID string) (Swarm, int64, string, *pipeline.Document, error) {
	w, err := s.GetSwarm(ctx, swarmID)
	if errors.Is(err, ErrNotFound) {
		return Swarm{}, 0, "", nil, ErrNoActorStep
	}
	if err != nil {
		return Swarm{}, 0, "", nil, err
	}
	// Рой без источника — только мимо запуска (тесты, базы до #198): его
	// шаги остаются пути AppendPipelineVisit.
	if w.State != "active" || w.Execution != "running" || w.Graph == nil || w.Source == nil {
		return Swarm{}, 0, "", nil, ErrNoActorStep
	}
	var sequence int64
	var node, state string
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,state FROM swarm_steps WHERE swarm_id=? ORDER BY sequence DESC LIMIT 1`, swarmID).Scan(&sequence, &node, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return Swarm{}, 0, "", nil, ErrNoActorStep
	}
	if err != nil {
		return Swarm{}, 0, "", nil, err
	}
	if state != "pending" {
		return Swarm{}, 0, "", nil, ErrNoActorStep
	}
	d, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return Swarm{}, 0, "", nil, err
	}
	if n, ok := d.Nodes[node]; !ok || n.Type != "actor" {
		return Swarm{}, 0, "", nil, fmt.Errorf("pending step %d is not an actor node", sequence)
	}
	return w, sequence, node, d, nil
}

// markStepAttesting записывает попытку в шаг: актор, инкарнация, request_id.
// Запись сама условна на pending: шаг, который уже не ждёт доставки, она не
// меняет (ErrNoActorStep), что бы ни показала выборка вызывающего.
func (s *Store) markStepAttesting(ctx context.Context, swarmID string, sequence int64, actor ManagedActor, requestID string) error {
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='attesting',started_at=COALESCE(started_at,?),actor_uid=?,actor_incarnation=?,request_id=?,detail=NULL WHERE swarm_id=? AND sequence=? AND state='pending'`,
		nowRFC3339(), actor.UID, actor.Incarnation, requestID, swarmID, sequence)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrNoActorStep
	}
	return nil
}

// refusePendingStep закрывает ждущий доставки шаг как refused с причиной и
// ставит в той же транзакции уведомление об отказе шага (#282).
func (s *Store) refusePendingStep(ctx context.Context, swarmID string, sequence int64, detail string) error {
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='refused',finished_at=?,detail=? WHERE swarm_id=? AND sequence=? AND state='pending'`, nowRFC3339(), stepDetail(detail), swarmID, sequence)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return err
	}
	if err := s.queueStepFailure(ctx, swarmID, sequence); err != nil {
		return err
	}
	_, err = s.ResolvePipelinePolicyReview(ctx, swarmID, sequence)
	return err
}

// Отказ review уже мог завершить проход внутри refusePendingStep. Обычный
// policy refusal завершает его здесь; pass_finished остаётся единственным.
func (s *Store) failPolicyPendingActor(ctx context.Context, swarmID string, sequence, pass int64, node, reason string) error {
	if err := s.refusePendingStep(ctx, swarmID, sequence, reason); err != nil {
		return err
	}
	w, err := s.GetSwarm(ctx, swarmID)
	if err != nil {
		return err
	}
	if w.Execution == "failed" {
		return nil
	}
	if err := s.advanceExecution(ctx, swarmID, "failed", nowRFC3339()); err != nil {
		return err
	}
	return s.queueTelegram(ctx, "pass_finished", TelegramEvent{Swarm: swarmID, Sequence: pass, Node: node, Execution: "failed"})
}

// queueStepFailure ставит уведомление step_failed о шаге, который только
// что стал refused или unconfirmed: узел и состояние — из самого шага, как
// их записала транзакция. Причину строка не копирует: шаг refused и
// unconfirmed ни одна запись больше не меняет, отправитель читает её из шага.
func (s *Store) queueStepFailure(ctx context.Context, swarmID string, sequence int64) error {
	ev := TelegramEvent{Swarm: swarmID, Sequence: sequence}
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state FROM swarm_steps WHERE swarm_id=? AND sequence=?`, swarmID, sequence).Scan(&ev.Node, &ev.State); err != nil {
		return err
	}
	return s.queueTelegram(ctx, "step_failed", ev)
}

// RecordActorStepAttestation records the verified workspace attestation of
// the attempt: origin and commit are those of the consumed claim. In one
// transaction it checks that the swarm is active and its graph running, that
// the claim names the swarm's origin and the commit of the step's pass
// (#229), that the step is still attesting for this request, actor and
// incarnation, and that the node's role is still bound to that actor at that
// incarnation. Only then the step becomes
// delivering with attested_commit. A stopped swarm or another attempt is
// ErrPipelineVisitConflict; another source is ErrPipelineSourceUnverified; a
// moved or stale role binding is its binding error. Nothing is written on
// refusal: the caller closes the step with RefuseActorStep.
func (s *Store) RecordActorStepAttestation(ctx context.Context, a ActorStepAttempt, origin, commit string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		w, err := s.currentAttempt(ctx, a, "attesting")
		if err != nil {
			return err
		}
		if w.Source == nil || w.Source.Origin != origin {
			return ErrPipelineSourceUnverified
		}
		// Commit прохода шага, а не роя (#229).
		if source, err := s.PipelineActorStepSource(ctx, a.SwarmID, a.Sequence); err != nil {
			return err
		} else if source.Commit == "" || source.Commit != commit {
			return ErrPipelineSourceUnverified
		}
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='delivering',attested_commit=? WHERE swarm_id=? AND sequence=? AND request_id=? AND state='attesting'`, commit, a.SwarmID, a.Sequence, a.RequestID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return ErrPipelineVisitConflict
		}
		return nil
	})
}

// CheckActorStepAttestation reports whether the attempt may still ask the
// guest for its workspace attestation: the swarm is active and running, the
// step is attesting for this request, actor and incarnation, and the role is
// still bound to that actor. The server calls it right before the guest call,
// after waiting for the actor's relay; it writes nothing.
func (s *Store) CheckActorStepAttestation(ctx context.Context, a ActorStepAttempt) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.currentAttempt(ctx, a, "attesting")
		return err
	})
}

// WithdrawActorStep undoes the reservation of an attempt that never reached
// the guest, as when the server shuts down before the attestation call: in
// one transaction the step goes back to pending without actor, incarnation,
// request ID, start or reason, and the attempt's workspace lease is released.
// The next dispatcher reserves the step anew. A step that is no longer
// attesting for this request is ErrPipelineVisitConflict and nothing changes.
func (s *Store) WithdrawActorStep(ctx context.Context, a ActorStepAttempt) error {
	if !validPipelineField(a.SwarmID) || !validPipelineField(a.RequestID) || a.Sequence < 1 {
		return errors.New("invalid actor step attempt")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='pending',started_at=NULL,actor_uid=NULL,actor_incarnation=NULL,request_id=NULL,detail=NULL WHERE swarm_id=? AND sequence=? AND request_id=? AND state='attesting'`, a.SwarmID, a.Sequence, a.RequestID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return ErrPipelineVisitConflict
		}
		return s.FinishActorOperation(ctx, a.Lease, true)
	})
}

// CheckActorStepDelivery reports whether the attempt may still be delivered:
// the swarm is active and running, the step is delivering for this request,
// actor and incarnation, and the role is still bound to that actor. The
// server calls it right before and after sending; it writes nothing.
func (s *Store) CheckActorStepDelivery(ctx context.Context, a ActorStepAttempt) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.currentAttempt(ctx, a, "delivering")
		return err
	})
}

// currentAttempt — общая проверка попытки в транзакции: рой активен и идёт,
// шаг в состоянии state той же попытки и актора, роль привязана к нему.
func (s *Store) currentAttempt(ctx context.Context, a ActorStepAttempt, state string) (Swarm, error) {
	w, err := s.GetSwarm(ctx, a.SwarmID)
	if err != nil {
		return Swarm{}, err
	}
	if w.State != "active" || !activeExecution(w.Execution) || w.Graph == nil {
		return Swarm{}, fmt.Errorf("%w: swarm is %s, execution %s", ErrPipelineVisitConflict, w.State, w.Execution)
	}
	var node, current, request, actor string
	var incarnation int64
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state,COALESCE(request_id,''),COALESCE(actor_uid,''),COALESCE(actor_incarnation,0) FROM swarm_steps WHERE swarm_id=? AND sequence=?`, a.SwarmID, a.Sequence).Scan(&node, &current, &request, &actor, &incarnation)
	if errors.Is(err, sql.ErrNoRows) {
		return Swarm{}, ErrPipelineVisitConflict
	}
	if err != nil {
		return Swarm{}, err
	}
	if current != state || request != a.RequestID || actor != a.Actor.UID || incarnation != a.Actor.Incarnation || node != a.NodeID {
		return Swarm{}, fmt.Errorf("%w: step %d is %s", ErrPipelineVisitConflict, a.Sequence, current)
	}
	d, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return Swarm{}, err
	}
	if err := s.requireRoleActor(ctx, a.SwarmID, d.Nodes[node].Role, actor, incarnation); err != nil {
		return Swarm{}, err
	}
	if _, assignment, err := s.inlinePolicyReview(ctx, a.SwarmID, a.Sequence); err == nil {
		source, err := s.PipelineActorStepSource(ctx, a.SwarmID, a.Sequence)
		if err != nil {
			return Swarm{}, err
		}
		if source != a.Source {
			return Swarm{}, ErrPipelineSourceUnverified
		}
		if err := s.CheckPipelinePolicyReviewAssignment(ctx, a.SwarmID, assignment.CandidateID, assignment.RuleID); err != nil {
			return Swarm{}, err
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Swarm{}, err
	}
	allowed, err := s.actorEntryPolicyAllowed(ctx, w, d, a.Sequence)
	if err != nil {
		return Swarm{}, err
	}
	if !allowed {
		return Swarm{}, ErrPipelinePolicyPending
	}
	return w, nil
}

// FinishActorStepDelivery records a confirmed delivery: the step becomes
// inflight and the actor reports its outcome with pipeline_visit_complete.
// Only a delivering step of this attempt changes; otherwise (the actor already
// answered, the swarm stopped, a replay) ErrPipelineVisitConflict.
func (s *Store) FinishActorStepDelivery(ctx context.Context, a ActorStepAttempt) error {
	return s.moveAttempt(ctx, a, []string{"delivering"}, "inflight", "", false)
}

// UnconfirmActorStep records that a guest effect of the attempt ended with an
// unknown outcome: the attestation or the instruction may have taken effect.
// The step becomes unconfirmed with the reason and keeps its actor, request ID
// and attestation: the graph does not advance, the attempt is not repeated
// and its outcome is not accepted. Only an attesting or delivering step of
// this attempt changes.
func (s *Store) UnconfirmActorStep(ctx context.Context, a ActorStepAttempt, detail string) error {
	return s.moveAttempt(ctx, a, []string{"attesting", "delivering"}, "unconfirmed", detail, false)
}

// RefuseActorStep closes an attempt whose instruction certainly was not sent:
// the attestation was refused or rejected, or delivery failed before the send.
// The step becomes refused with the reason; the graph does not advance. Only
// an attesting or delivering step of this attempt changes.
func (s *Store) RefuseActorStep(ctx context.Context, a ActorStepAttempt, detail string) error {
	return s.moveAttempt(ctx, a, []string{"attesting", "delivering"}, "refused", detail, true)
}

func (s *Store) moveAttempt(ctx context.Context, a ActorStepAttempt, from []string, to, detail string, finished bool) error {
	if !validPipelineField(a.SwarmID) || !validPipelineField(a.RequestID) || a.Sequence < 1 {
		return errors.New("invalid actor step attempt")
	}
	var reason any
	if detail != "" {
		reason = stepDetail(detail)
	}
	var finishedAt any
	if finished {
		finishedAt = nowRFC3339()
	}
	// Шаг без исхода (refused, unconfirmed) — отказ шага в Telegram (#282),
	// в одной транзакции с переходом.
	return s.WithTx(ctx, func(ctx context.Context) error {
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state=?,detail=?,finished_at=COALESCE(?,finished_at) WHERE swarm_id=? AND sequence=? AND request_id=? AND state IN (?,?)`,
			to, reason, finishedAt, a.SwarmID, a.Sequence, a.RequestID, from[0], from[len(from)-1])
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return ErrPipelineVisitConflict
		}
		if to != "refused" && to != "unconfirmed" {
			return nil
		}
		if err := s.queueStepFailure(ctx, a.SwarmID, a.Sequence); err != nil {
			return err
		}
		if to == "refused" {
			_, err := s.ResolvePipelinePolicyReview(ctx, a.SwarmID, a.Sequence)
			return err
		}
		return nil
	})
}

// RecoverActorSteps marks attempts whose guest effect may have been cut by the
// previous server process as unconfirmed: an attestation (attesting) or an
// instruction being sent (delivering). Neither is repeated. Delivered
// (inflight) and pending steps keep their state: the actor still reports, the
// dispatcher still delivers. Call only during exclusive server startup. Each
// recovered step queues its step_failed Telegram notification in the same
// transaction (#282).
func (s *Store) RecoverActorSteps(ctx context.Context) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		rows, err := s.conn(ctx).QueryContext(ctx, `UPDATE swarm_steps SET detail=CASE state WHEN 'attesting' THEN ? ELSE ? END,state='unconfirmed' WHERE state IN ('attesting','delivering') RETURNING swarm_id,sequence`, detailRestartAttestation, detailRestartDelivery)
		if err != nil {
			return err
		}
		type step struct {
			swarm    string
			sequence int64
		}
		var recovered []step
		for rows.Next() {
			var st step
			if err := rows.Scan(&st.swarm, &st.sequence); err != nil {
				rows.Close()
				return err
			}
			recovered = append(recovered, st)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// RETURNING порядка не обещает: уведомления идут по рою и шагу.
		sort.Slice(recovered, func(i, j int) bool {
			if recovered[i].swarm != recovered[j].swarm {
				return recovered[i].swarm < recovered[j].swarm
			}
			return recovered[i].sequence < recovered[j].sequence
		})
		for _, st := range recovered {
			if err := s.queueStepFailure(ctx, st.swarm, st.sequence); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingActorStepSwarms lists the swarms whose latest step waits for
// delivery: a pending actor step of an active running swarm with a source.
func (s *Store) PendingActorStepSwarms(ctx context.Context) ([]string, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT w.id FROM swarms w JOIN pipeline_sources p ON p.swarm_id=w.id JOIN swarm_steps st ON st.swarm_id=w.id WHERE w.state='active' AND w.execution='running' AND w.graph IS NOT NULL AND st.state='pending' AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=w.id) ORDER BY w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

// stepDetail приводит причину к пределу схемы: валидный UTF-8 без
// управляющих символов, не длиннее maxStepDetailBytes.
func stepDetail(detail string) string {
	detail = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(detail, "?"))
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "unspecified"
	}
	if len(detail) <= maxStepDetailBytes {
		return detail
	}
	cut := maxStepDetailBytes - len("...")
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut] + "..."
}
