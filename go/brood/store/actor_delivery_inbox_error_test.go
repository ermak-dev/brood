package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInboxAttestationFailureIdentifiesSafeStageWithoutLeakingCause(t *testing.T) {
	for _, stage := range []string{"intent", "receiver"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s := dispatchStore(t)
			a := reserveOne(t, s, "fixture-inbox-stage")
			in := instructionDeliveryInput(a)
			receiver := strings.Repeat("b", 64)
			const internal = "synthetic-private-cause"
			if stage == "intent" {
				_, err := s.db.Exec(`CREATE TRIGGER refuse_fixture_inbox_intent BEFORE INSERT ON actor_deliveries BEGIN SELECT RAISE(ABORT,'` + internal + `'); END`)
				mustNil(t, err)
			} else {
				receiver = in.JournalID
			}
			_, err := s.RecordActorStepInboxAttestation(ctx, a, startOrigin, startCommit, in, receiver, allow)
			var failure interface{ InboxCommitStage() string }
			if err == nil || !errors.As(err, &failure) || failure.InboxCommitStage() != stage || strings.Contains(err.Error(), internal) {
				t.Fatalf("commit error did not retain a safe stage %s: %T", stage, err)
			}
			if stage == "receiver" && !errors.Is(err, ErrActorDeliveryConflict) {
				t.Fatal("receiver conflict no longer has its typed cause")
			}
			if stepOf(t, s, 1).State != "attesting" {
				t.Fatal("refusal committed the step before the delivery was pinned")
			}
			if _, err := s.GetActorDeliveryForSource(ctx, a.SwarmID, "step", "1"); !errors.Is(err, ErrNotFound) {
				t.Fatal("refusal left a partial delivery intent")
			}
		})
	}
}
