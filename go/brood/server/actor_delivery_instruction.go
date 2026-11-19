package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func preparedInstructionDelivery(a store.ActorStepAttempt, text, journal string) (store.ActorDeliveryInput, error) {
	if len(journal) != 64 || strings.ToLower(journal) != journal {
		return store.ActorDeliveryInput{}, store.ErrActorDeliveryConflict
	}
	if _, err := hex.DecodeString(journal); err != nil {
		return store.ActorDeliveryInput{}, store.ErrActorDeliveryConflict
	}
	payload, err := json.Marshal(relay.MessageRequest{To: a.Actor.Name, From: stepMessageSender(a.SwarmID), Body: text})
	if err != nil {
		return store.ActorDeliveryInput{}, err
	}
	in := store.ActorDeliveryInput{SwarmID: a.SwarmID, SourceKind: "step", SourceID: strconv.FormatInt(a.Sequence, 10), RequestID: a.RequestID, Commit: a.Source.Commit, ActorUID: a.Actor.UID, Incarnation: a.Actor.Incarnation, AtespaceUID: a.Actor.AtespaceUID, JournalID: journal, Payload: string(payload)}
	// Верхняя граница ID протокола проверяется до эффекта workspace_attest.
	if _, err := actorDeliveryMessage(store.ActorDeliveryIntent{ID: strings.Repeat("d", 128), Input: in}, "probe"); err != nil {
		return store.ActorDeliveryInput{}, err
	}
	return in, nil
}

func (d *actorDeliveryService) instructionAuthority(ctx context.Context, in store.ActorDeliveryInput) bool {
	s := d.server
	actor, swarm, err := s.managed.authorizeCommandActorDurable(ctx, store.ManagedActor{UID: in.ActorUID, Incarnation: in.Incarnation, SwarmID: in.SwarmID, AtespaceUID: in.AtespaceUID})
	if err != nil {
		return false
	}
	owner := actor.DelegatorID
	if owner == "" {
		current, err := s.store.GetSwarm(ctx, swarm.ID)
		if err != nil {
			return false
		}
		owner = current.OwnerID
	}
	return s.store.CheckActorDeliveryManager(ctx, in.SwarmID, owner) == nil
}

func (d *actorDeliveryService) instruction(ctx context.Context, a store.ActorStepAttempt) bool {
	intent, err := d.server.store.GetActorDeliveryForSource(ctx, a.SwarmID, "step", strconv.FormatInt(a.Sequence, 10))
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		d.server.steps.settle(ctx, a, true, "retained instruction delivery is unavailable; no legacy resend was attempted")
		return true
	}
	unlock := d.server.deliveries.lock(intent.ID)
	defer unlock()
	call, cancel := context.WithTimeout(ctx, requestDeliveryTimeout)
	defer cancel()
	d.deliverInstruction(call, intent)
	return true
}

func (d *actorDeliveryService) recoverStep(parent context.Context, candidate store.ActorDeliveryCandidate) {
	s := d.server
	schedule, err := s.store.GetActorDeliverySchedule(parent, candidate.ID)
	if err != nil || !schedule.Confirmed && !schedule.Exhausted && d.now().Before(schedule.NotBefore) {
		return
	}
	intent, err := s.store.GetActorDelivery(parent, candidate.ID)
	if err != nil {
		return
	}
	if !schedule.Confirmed && !schedule.Exhausted && (s.managed.hub == nil || actorDeliveryJournal(s.managed.hub, intent.Input.ActorUID, intent.Input.Incarnation) != intent.Input.JournalID || !s.managed.hub.Connected(intent.Input.ActorUID, intent.Input.Incarnation)) {
		return
	}
	unlock, ok := s.deliveries.tryLock(intent.ID)
	if !ok {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(parent, requestDeliveryTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	if schedule.Exhausted {
		_ = s.store.RecordActorDeliveryWait(ctx, intent.ID, durableAnswerBudget, func(ctx context.Context) bool { return d.instructionAuthority(ctx, intent.Input) })
		return
	}
	d.deliverInstruction(ctx, intent)
}

func (d *actorDeliveryService) deliverInstruction(ctx context.Context, intent store.ActorDeliveryIntent) {
	local := func(ctx context.Context) bool { return d.instructionAuthority(ctx, intent.Input) }
	outcome, err := d.attempt(ctx, intent, local)
	if errors.Is(err, errActorDeliveryInboxProtected) || err == nil && (outcome == "delivered" || outcome == "duplicate") {
		return
	}
	detail := "instruction delivery outcome is unknown; only a read-only receipt probe is permitted"
	if outcome == "not_sent" && err == nil {
		detail = "instruction was not sent; waiting for the next persisted delivery attempt"
	}
	if errors.Is(err, store.ErrActorDeliveryPending) {
		detail = "instruction delivery is waiting for persisted deadline or backoff"
	}
	if errors.Is(err, store.ErrActorDeliveryBudget) {
		detail = durableAnswerBudget
	}
	if errors.Is(err, relay.ErrDeliveryJournalChanged) {
		detail = "receiver journal changed; instruction was not resent"
	}
	book, cancel := bookkeeping(ctx)
	defer cancel()
	_ = d.server.store.RecordActorDeliveryWait(book, intent.ID, detail, local)
}
