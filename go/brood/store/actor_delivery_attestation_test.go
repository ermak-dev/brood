package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func instructionDeliveryInput(a ActorStepAttempt) ActorDeliveryInput {
	return ActorDeliveryInput{SwarmID: a.SwarmID, SourceKind: "step", SourceID: strconv.FormatInt(a.Sequence, 10), RequestID: a.RequestID, Commit: a.Source.Commit, ActorUID: a.Actor.UID, Incarnation: a.Actor.Incarnation, AtespaceUID: a.Actor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"to":"a","body":"build","from":"swarm:one","waitSec":0}`}
}

func TestInstructionAttestationAndIntentCommitTogether(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "atomic-instruction")
	intent, err := s.RecordActorStepAttestationDelivery(ctx, a, startOrigin, startCommit, instructionDeliveryInput(a), allow)
	mustNil(t, err)
	if stepOf(t, s, 1).State != "delivering" || intent.Input.RequestID != a.RequestID || intent.Input.Commit != startCommit {
		t.Fatal("attestation did not pin instruction intent")
	}
	stored, err := s.GetActorDeliveryForSource(ctx, "one", "step", "1")
	mustNil(t, err)
	if stored != intent {
		t.Fatal("delivering visit has no exact retained payload")
	}
	if _, err := s.CurrentActorOperation(ctx, a.Actor.UID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deliverable instruction retained its already verified attestation lease")
	}
}

func TestInboxInstructionPinsReceiverInAttestationCommit(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "atomic-inbox-instruction")
	input := instructionDeliveryInput(a)
	if _, err := s.RecordActorStepInboxAttestation(ctx, a, startOrigin, startCommit, input, input.JournalID, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("native journal used as inbox receiver: %v", err)
	}
	if stepOf(t, s, 1).State != "attesting" {
		t.Fatal("refused selection partially committed the attestation")
	}
	if _, err := s.GetActorDeliveryForSource(ctx, "one", "step", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused selection left a recoverable native intent: %v", err)
	}
	if lease, err := s.CurrentActorOperation(ctx, a.Actor.UID); err != nil || lease != a.Lease {
		t.Fatalf("refused selection cleared the workspace lease: %+v %v", lease, err)
	}
	receiver := strings.Repeat("b", 64)
	intent, err := s.RecordActorStepInboxAttestation(ctx, a, startOrigin, startCommit, input, receiver, allow)
	mustNil(t, err)
	state, err := s.ActorDeliveryInboxStatus(ctx, intent.ID)
	mustNil(t, err)
	if state.Target.ReceiverID != receiver || state.Target.ActorUID != input.ActorUID || state.Custody || state.DispatchReserved || stepOf(t, s, 1).State != "delivering" {
		t.Fatalf("attestation did not atomically pin inbox recovery: %+v", state)
	}
	now := time.Now().UTC()
	if _, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("recovery could reserve native delivery for a pinned inbox: %v", err)
	}
}

func TestRecoveredPreparedInstructionWithoutAttemptIsNotAnUncertainSend(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "prepared-instruction")
	intent, err := s.RecordActorStepAttestationDelivery(ctx, a, startOrigin, startCommit, instructionDeliveryInput(a), allow)
	mustNil(t, err)
	mustNil(t, s.RecoverActorSteps(ctx))
	now := time.Now().UTC()
	ticket, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	if ticket.Mode != "send" || ticket.Number != 1 {
		t.Fatal("coarse step recovery invented an external send absent from durable attempt history")
	}
}

func TestRetainedInstructionCandidatesDoNotRepeatAttestationOrLegacyDelivery(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "retained-instruction")
	intent, err := s.RecordActorStepAttestationDelivery(ctx, a, startOrigin, startCommit, instructionDeliveryInput(a), allow)
	mustNil(t, err)
	mustNil(t, s.UnconfirmActorStep(ctx, a, "response lost"))
	candidates, err := s.PendingActorDeliverySteps(ctx, "", 16)
	mustNil(t, err)
	if len(candidates) != 1 || candidates[0].ID != intent.ID || candidates[0].SourceID != "1" {
		t.Fatal("saved instruction was not a recovery candidate")
	}
	_, err = s.db.Exec(`UPDATE swarms SET execution='stopped' WHERE id='one'`)
	mustNil(t, err)
	candidates, err = s.PendingActorDeliverySteps(ctx, "", 16)
	mustNil(t, err)
	if len(candidates) != 0 {
		t.Fatal("stopped source remained eligible for automatic instruction recovery")
	}
	legacy := dispatchStore(t)
	old := attestOne(t, legacy, "legacy-instruction")
	mustNil(t, legacy.UnconfirmActorStep(ctx, old, "response lost"))
	candidates, err = legacy.PendingActorDeliverySteps(ctx, "", 16)
	mustNil(t, err)
	if len(candidates) != 0 {
		t.Fatal("legacy unknown became an automatic resend candidate")
	}
}

func TestInstructionWaitingUsesStoredEvidenceWithoutResettingUnknown(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "waiting-instruction")
	intent, err := s.RecordActorStepAttestationDelivery(ctx, a, startOrigin, startCommit, instructionDeliveryInput(a), allow)
	mustNil(t, err)
	now := time.Now().UTC()
	first, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, first, "not_sent", "not ready", now, allow))
	mustNil(t, s.RecordActorDeliveryWait(ctx, intent.ID, "waiting for session", allow))
	if stepOf(t, s, 1).State != "delivering" {
		t.Fatal("known not-sent was converted into an uncertain send")
	}
	now = now.Add(time.Hour)
	second, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	if second.Mode != "send" {
		t.Fatal("known safe retry became an unknown probe")
	}
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, second, "unconfirmed", "reply lost", now, allow))
	mustNil(t, s.RecordActorDeliveryWait(ctx, intent.ID, "only read-only recovery", allow))
	if stepOf(t, s, 1).State != "unconfirmed" {
		t.Fatal("uncertain send did not hold the source")
	}
}

func TestExhaustedInstructionStillProjectsBudgetAfterCrash(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "exhausted-instruction")
	intent, err := s.RecordActorStepAttestationDelivery(ctx, a, startOrigin, startCommit, instructionDeliveryInput(a), allow)
	mustNil(t, err)
	now := time.Now().UTC()
	for range MaxActorDeliveryAttempts {
		ticket, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
		mustNil(t, err)
		mustNil(t, s.RecordActorDeliveryOutcome(ctx, ticket, "unconfirmed", "receipt unknown", now, allow))
		now = now.Add(time.Hour)
	}
	candidates, err := s.PendingActorDeliverySteps(ctx, "", 16)
	mustNil(t, err)
	if len(candidates) != 1 {
		t.Fatal("crash before source bookkeeping hid exhausted instruction")
	}
	mustNil(t, s.RecordActorDeliveryWait(ctx, intent.ID, "waiting", allow))
	visit := stepOf(t, s, 1)
	if visit.State != "unconfirmed" || !strings.Contains(visit.Detail, "budget exhausted") {
		t.Fatal("budget projection lost unknown source or its reason")
	}
	candidates, err = s.PendingActorDeliverySteps(ctx, "", 16)
	mustNil(t, err)
	if len(candidates) != 0 {
		t.Fatal("already projected exhaustion remained an active retry candidate")
	}
}

func TestInstructionIntentFailureRollsBackVerifiedAttestationRecord(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "atomic-instruction")
	_, err := s.db.Exec(`CREATE TRIGGER fail_instruction_intent BEFORE INSERT ON actor_deliveries BEGIN SELECT RAISE(ABORT,'injected intent storage failure'); END`)
	mustNil(t, err)
	if _, err := s.RecordActorStepAttestationDelivery(ctx, a, startOrigin, startCommit, instructionDeliveryInput(a), allow); err == nil {
		t.Fatal("failed intent returned delivery permission")
	}
	visit := stepOf(t, s, 1)
	if visit.State != "attesting" || visit.AttestedCommit != "" {
		t.Fatal("verified attestation escaped without retained intent")
	}
	var intents int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM actor_deliveries`).Scan(&intents))
	if intents != 0 {
		t.Fatal("failed transaction retained a partial intent")
	}
}

func TestInstructionAttestationIntentRejectsDifferentScopeAndAuthority(t *testing.T) {
	for _, change := range []string{"actor", "request", "commit", "authority"} {
		t.Run(change, func(t *testing.T) {
			s := dispatchStore(t)
			a := reserveOne(t, s, "atomic-instruction")
			in := instructionDeliveryInput(a)
			authorize := allow
			switch change {
			case "actor":
				in.ActorUID = "b"
			case "request":
				in.RequestID = "different"
			case "commit":
				in.Commit = strings.Repeat("f", 40)
			case "authority":
				authorize = func(context.Context) bool { return false }
			}
			if _, err := s.RecordActorStepAttestationDelivery(context.Background(), a, startOrigin, startCommit, in, authorize); err == nil {
				t.Fatal("foreign intent opened instruction delivery")
			}
			if stepOf(t, s, 1).State != "attesting" {
				t.Fatal("rejected atomic intent changed source")
			}
		})
	}
}
