package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func actorDeliveryFixture(t *testing.T) (*Store, ActorDeliveryInput) {
	t.Helper()
	s, _ := requestStore(t)
	req := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, req, "u", "", "use sqlite"))
	return s, ActorDeliveryInput{SwarmID: "s", SourceKind: "answer", SourceID: req.ID, ActorUID: "worker", Incarnation: 1, AtespaceUID: "native-s", JournalID: strings.Repeat("a", 64), Payload: `{"to":"worker","body":"use sqlite","from":"operator:u","waitSec":0}`}
}

func TestActorDeliverySourceLookupAndCurrentFence(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	got, err := s.GetActorDeliveryForSource(ctx, in.SwarmID, in.SourceKind, in.SourceID)
	mustNil(t, err)
	if got != intent {
		t.Fatal("source lookup lost retained identity")
	}
	if _, err := s.GetActorDeliveryForSource(ctx, "foreign", in.SourceKind, in.SourceID); !errors.Is(err, ErrNotFound) {
		t.Fatal("source lookup crossed swarm scope")
	}
	mustNil(t, s.CheckActorDeliveryCurrent(ctx, intent.ID))
	_, err = s.db.Exec(`UPDATE swarms SET atespace_uid='replacement' WHERE id='s'`)
	mustNil(t, err)
	if err := s.CheckActorDeliveryCurrent(ctx, intent.ID); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatal("final delivery fence ignored namespace replacement")
	}
}

func TestActorDeliveryIntentPinsSourcePayloadAndJournal(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	first, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	again, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	if first.ID == "" || again != first || first.Input != in {
		t.Fatal("exact preparation changed retained delivery")
	}
	for _, field := range []string{"payload", "journal", "incarnation", "namespace"} {
		changed := in
		switch field {
		case "payload":
			changed.Payload = `{"body":"different"}`
		case "journal":
			changed.JournalID = strings.Repeat("b", 64)
		case "incarnation":
			changed.Incarnation++
		case "namespace":
			changed.AtespaceUID = "foreign"
		}
		if _, err := s.PrepareActorDelivery(ctx, changed, allow); err == nil {
			t.Errorf("changed %s rebound retained delivery", field)
		}
	}
	if _, err := s.PrepareActorDelivery(ctx, in, func(context.Context) bool { return false }); !errors.Is(err, ErrActorDeliveryDenied) {
		t.Fatal("preparation bypassed current authority")
	}
}

func TestActorDeliveryCrashCanOnlyReserveBoundedReadOnlyProbes(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	first, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	if first.Number != 1 || first.Mode != "send" {
		t.Fatal("first reservation was not one send")
	}
	if _, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow); !errors.Is(err, ErrActorDeliveryPending) {
		t.Fatal("live attempt bypassed deadline/backoff")
	}
	var seq int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	for number := 2; number <= MaxActorDeliveryAttempts; number++ {
		now = now.Add(20 * time.Minute)
		attempt, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
		mustNil(t, err)
		if attempt.Mode != "probe" || attempt.Number != number {
			t.Fatalf("crash/unknown gave resend permission: %+v", attempt)
		}
		if err := s.RecordActorDeliveryOutcome(ctx, attempt, "not_sent", "old receipt", now, allow); !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatal("probe cleared prior uncertainty with historical not-sent")
		}
		mustNil(t, s.RecordActorDeliveryOutcome(ctx, attempt, "unconfirmed", "no retained receipt", now, allow))
	}
	now = now.Add(20 * time.Minute)
	if _, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow); !errors.Is(err, ErrActorDeliveryBudget) {
		t.Fatal("automatic probe budget was not bounded")
	}
}

func TestActorDeliveryKnownNotSentCanRetryAndDuplicateStopsAttempts(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	first, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, first, "not_sent", "not ready", now, allow))
	now = now.Add(time.Hour)
	second, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	if second.Mode != "send" {
		t.Fatal("current known not-sent lost safe retry")
	}
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, second, "unconfirmed", "reply lost", now, allow))
	now = now.Add(time.Hour)
	third, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	if third.Mode != "probe" {
		t.Fatal("unknown retry became another send")
	}
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, third, "duplicate", "prior delivery confirmed", now, allow))
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, third, "duplicate", "prior delivery confirmed", now.Add(time.Second), allow))
	if err := s.RecordActorDeliveryOutcome(ctx, third, "unconfirmed", "changed", now, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatal("retained receipt changed")
	}
	now = now.Add(time.Hour)
	if _, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow); !errors.Is(err, ErrActorDeliveryComplete) {
		t.Fatal("confirmed duplicate authorized another attempt")
	}
	history, err := s.GetActorDeliveryHistory(ctx, intent.ID)
	mustNil(t, err)
	if len(history) != 3 || history[0].Outcome != "not_sent" || history[1].Outcome != "unconfirmed" || history[2].Outcome != "duplicate" || history[2].Attempt.Number != 3 {
		t.Fatalf("immutable transport history lost: %+v", history)
	}
}

func TestActorDeliveryStepRequiresAttestationAndCurrentRoleBinding(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	attempt := reserveOne(t, s, "delivery-request")
	actor, err := s.GetManagedActor(ctx, "a")
	mustNil(t, err)
	in := ActorDeliveryInput{SwarmID: "one", SourceKind: "step", SourceID: "1", ActorUID: "a", Incarnation: actor.Incarnation, AtespaceUID: actor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"to":"a","from":"swarm:one","body":"build","waitSec":0}`}
	if _, err := s.PrepareActorDelivery(ctx, in, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatal("unattested step prepared delivery")
	}
	mustNil(t, s.RecordActorStepAttestation(ctx, attempt, startOrigin, startCommit))
	in.RequestID, in.Commit = attempt.RequestID, startCommit
	wrong := in
	wrong.RequestID = "other-attempt"
	if _, err := s.PrepareActorDelivery(ctx, wrong, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatal("delivery ignored exact attestation request identity")
	}
	wrong = in
	wrong.Commit = strings.Repeat("f", 40)
	if _, err := s.PrepareActorDelivery(ctx, wrong, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatal("delivery ignored exact attested commit")
	}
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b", ActorIncarnation: 1}))
	now := time.Now().UTC()
	if ticket, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow); err == nil {
		t.Fatalf("changed role still received effect permission: %+v", ticket)
	}
}

func TestActorDeliveryConcurrentReservationsIssueOneSendTicket(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Now().UTC()
	results := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	issued := 0
	for err := range results {
		if err == nil {
			issued++
		} else if !errors.Is(err, ErrActorDeliveryPending) {
			t.Fatal(err)
		}
	}
	if issued != 1 {
		t.Fatalf("issued %d send tickets for one logical message", issued)
	}
	history, err := s.GetActorDeliveryHistory(ctx, intent.ID)
	mustNil(t, err)
	if len(history) != 1 || history[0].Attempt.Mode != "send" || history[0].Outcome != "" {
		t.Fatal("concurrent reservations lost uncertain history")
	}
}

func TestActorDeliveryOutcomeRechecksAuthorityAndNamespace(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Now().UTC()
	ticket, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	if err := s.RecordActorDeliveryOutcome(ctx, ticket, "delivered", "confirmed", now, func(context.Context) bool { return false }); !errors.Is(err, ErrActorDeliveryDenied) {
		t.Fatal("receipt recorded after authority revocation")
	}
	_, err = s.db.Exec(`UPDATE swarms SET atespace_uid='replacement-namespace' WHERE id='s'`)
	mustNil(t, err)
	if err := s.RecordActorDeliveryOutcome(ctx, ticket, "delivered", "confirmed", now, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatal("receipt crossed namespace replacement")
	}
	history, err := s.GetActorDeliveryHistory(ctx, intent.ID)
	mustNil(t, err)
	if len(history) != 1 || history[0].Outcome != "" {
		t.Fatal("revoked receipt changed uncertain history")
	}
}

func TestActorDeliveryReservationChecksCurrentSourceAndAuthority(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Now().UTC()
	if _, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), func(context.Context) bool { return false }); !errors.Is(err, ErrActorDeliveryDenied) {
		t.Fatal("reservation ignored revoked authority")
	}
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
		if !errors.Is(err, ErrDurableTransactionNested) {
			t.Fatal("effect ticket escaped before outer commit")
		}
		return nil
	}))
	_, err = s.db.Exec(`UPDATE swarms SET state='stopping' WHERE id='s'`)
	mustNil(t, err)
	if _, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("stopped source gained delivery permission: %v", err)
	}
}
