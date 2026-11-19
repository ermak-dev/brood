package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// Callback применяется повторно внутри FULL-транзакции Store. start включает
// обе текущие durable identity, delegation и запрет автоматической работы
// после owner-вмешательства; ручные права владельца он не ограничивает.
func (s *Server) ephemeralDeliveryAuthorize(userID string, parent store.SwarmChildParent) store.SwarmChildAuthorize {
	return func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if s.managed == nil || a.Action != "prepare_ephemeral_delivery" || a.Parent != parent {
			return store.ErrSwarmChildDenied
		}
		if err := s.authorizeSwarmOwnerIntent(ctx, userID, parent.Principal.SwarmID); err != nil {
			return err
		}
		a.Action = "start"
		return s.managed.childAuthority(ctx, a)
	}
}

func (d *actorDeliveryService) childAuthority(ctx context.Context, userID string, intent store.ActorDeliveryIntent) bool {
	if intent.Input.SourceKind != "child" {
		return false
	}
	job, err := d.server.store.GetEphemeralChildIntentForTask(ctx, intent.Input.SourceID)
	if err != nil {
		return false
	}
	work, err := d.server.store.SwarmChildWorkByID(ctx, intent.Input.SourceID)
	if err != nil || work.Parent != job.Parent {
		return false
	}
	return d.server.ephemeralDeliveryAuthorize(userID, job.Parent)(ctx, store.SwarmChildAuthorization{Action: "prepare_ephemeral_delivery", Parent: job.Parent, Caller: job.Parent.Principal, Executor: work.Task.Executor, RequestID: job.RequestID, Payload: job.Payload}) == nil
}

// Подготовка закрыта от публичного fan-out: вызывающий уже обязан иметь точные
// immutable intent, allocation, proof и registered child. Retained payload читаем
// первым: retry не рендерит текст и никогда не повторяет bootstrap/attestation.
func (s *Server) prepareEphemeralChildDelivery(ctx context.Context, userID string, parent store.SwarmChildParent, intentID string) (store.ActorDeliveryIntent, error) {
	job, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return store.ActorDeliveryIntent{}, err
	}
	if job.Parent != parent || s.managed == nil || s.managed.hub == nil || s.managed.deliverMessage == nil {
		return store.ActorDeliveryIntent{}, store.ErrSwarmChildDenied
	}
	allocation, err := s.store.GetEphemeralChildAllocation(ctx, intentID)
	if err != nil {
		return store.ActorDeliveryIntent{}, err
	}
	join, err := s.store.GetSwarmChildJoin(ctx, parent, func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if err := s.authorizeSwarmOwnerIntent(ctx, userID, parent.Principal.SwarmID); err != nil {
			return err
		}
		return s.managed.childAuthority(ctx, a)
	})
	if err != nil {
		return store.ActorDeliveryIntent{}, err
	}
	work := store.SwarmChildWork{Parent: parent}
	for _, child := range join.Children {
		if child.RequestID == job.RequestID {
			work.Task = child
			break
		}
	}
	membership, err := s.store.GetEphemeralChildIntentForTask(ctx, work.Task.ID)
	if err != nil {
		return store.ActorDeliveryIntent{}, err
	}
	if membership.ID != job.ID {
		return store.ActorDeliveryIntent{}, store.ErrSwarmChildDenied
	}
	swarm, err := s.store.GetSwarm(ctx, parent.Principal.SwarmID)
	if err != nil {
		return store.ActorDeliveryIntent{}, err
	}
	// Единственное внешнее namespace preflight до FULL-транзакции.
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return store.ActorDeliveryIntent{}, canceled
		}
		return store.ActorDeliveryIntent{}, err
	}
	actor, err := s.store.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return store.ActorDeliveryIntent{}, err
	}
	retained, err := s.store.GetActorDeliveryForSource(ctx, parent.Principal.SwarmID, "child", work.Task.ID)
	var in store.ActorDeliveryInput
	if err == nil {
		in = retained.Input
	} else if errors.Is(err, store.ErrNotFound) {
		proof, err := s.store.GetEphemeralChildAttestation(ctx, intentID)
		if err != nil {
			return store.ActorDeliveryIntent{}, err
		}
		text, err := ephemeralChildInstruction(ephemeralInstructionSource{Intent: job, Allocation: allocation, Proof: proof, Actor: actor, Work: work})
		if err != nil {
			return store.ActorDeliveryIntent{}, err
		}
		// Та же валидация journal и обоих protocol frames, что у graph instructions.
		prepared, err := preparedInstructionDelivery(store.ActorStepAttempt{SwarmID: parent.Principal.SwarmID, Actor: actor}, text, actorDeliveryJournal(s.managed.hub, actor.UID, actor.Incarnation))
		if err != nil {
			return store.ActorDeliveryIntent{}, err
		}
		in = prepared
		in.SourceKind, in.SourceID, in.RequestID, in.Commit = "child", work.Task.ID, job.RequestID, job.SourceCommit
	} else {
		return store.ActorDeliveryIntent{}, err
	}
	if !s.managed.hub.Connected(in.ActorUID, in.Incarnation) {
		return store.ActorDeliveryIntent{}, errRecipientOffline
	}
	if actorDeliveryJournal(s.managed.hub, in.ActorUID, in.Incarnation) != in.JournalID {
		return store.ActorDeliveryIntent{}, relay.ErrDeliveryJournalChanged
	}
	// Проверяем и retained frame без перестройки payload. FULL-резерв сам
	// сверяет все исходные координаты, proof и текущую authority под tx ctx.
	if _, err := actorDeliveryMessage(store.ActorDeliveryIntent{ID: strings.Repeat("d", 128), Input: in}, "probe"); err != nil {
		return store.ActorDeliveryIntent{}, err
	}
	return s.store.PrepareEphemeralChildDelivery(ctx, intentID, in, s.ephemeralDeliveryAuthorize(userID, parent))
}

func (s *Server) deliverEphemeralChild(parentCtx context.Context, userID string, parent store.SwarmChildParent, intentID string) (store.ActorDeliveryIntent, string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, requestDeliveryTimeout)
	defer cancel()
	intent, err := s.prepareEphemeralChildDelivery(ctx, userID, parent, intentID)
	if err != nil {
		return store.ActorDeliveryIntent{}, "", err
	}
	// Фоновый worker берёт тот же retained ID, не child/request/actor alias.
	unlock, err := s.actorDelivery.lockDelivery(ctx, intent.ID)
	if err != nil {
		return intent, "", err
	}
	defer unlock()
	outcome, err := s.actorDelivery.attempt(ctx, intent, func(ctx context.Context) bool { return s.actorDelivery.childAuthority(ctx, userID, intent) })
	return intent, outcome, err
}

// Существующие deliveryLocks не имеют cancellable wait. Не оставляем goroutine
// ждать mutex после отмены; короткий bounded retry использует тот же tryLock,
// что worker, и не меняет порядок/права каких-либо внешних эффектов.
func (d *actorDeliveryService) lockDelivery(ctx context.Context, id string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if unlock, ok := d.server.deliveries.tryLock(id); ok {
			return unlock, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (d *actorDeliveryService) recoverChildren(ctx context.Context, after string) (string, error) {
	return d.recoverPage(ctx, after, d.server.store.PendingActorDeliveryChildren, d.recoverChild)
}

func (d *actorDeliveryService) recoverChild(parent context.Context, candidate store.ActorDeliveryCandidate) {
	s := d.server
	unlock, ok := s.deliveries.tryLock(candidate.ID)
	if !ok {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(parent, requestDeliveryTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	schedule, err := s.store.GetActorDeliverySchedule(ctx, candidate.ID)
	if err != nil || schedule.Confirmed || schedule.Exhausted || d.now().Before(schedule.NotBefore) {
		return
	}
	intent, err := s.store.GetActorDelivery(ctx, candidate.ID)
	if err != nil || intent.Input.SourceKind != "child" {
		return
	}
	job, err := s.store.GetEphemeralChildIntentForTask(ctx, intent.Input.SourceID)
	if err != nil {
		return
	}
	actor, err := s.store.GetManagedActor(ctx, job.Parent.Principal.ActorUID)
	if err != nil || actor.DelegatorID == "" {
		return
	}
	// Восстановление использует текущего approving admin родителя, а не worker
	// admin и не сохранённый старый privilege. Никакой реконструкции сообщений.
	_, _ = d.attempt(ctx, intent, func(ctx context.Context) bool { return d.childAuthority(ctx, actor.DelegatorID, intent) })
}
