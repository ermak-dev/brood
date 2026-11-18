package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// BUG: Освобождённый владельцем lease старой попытки блокирует startup recovery.
// HYPOTHESIS: Recovery безусловно завершает старый lease, даже если его уже нет
// или workspace занимает другая операция. История не должна переигрывать эффект.
// SOLUTION: Recovery меняет только точно совпадающий retained lease;
// освобождённый hold не восстанавливается, операция-преемник не затрагивается.
// RESULT: FIXED
// STATUS: FIXED
func TestEphemeralAttestationRecoveryPreservesExplicitLeaseReleaseAndSuccessor(t *testing.T) {
	for _, successor := range []bool{false, true} {
		name := "released"
		if successor {
			name = "successor"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, intent := ephemeralAttestationFixture(t, true)
			_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
			mustNil(t, err)
			old, err := s.CurrentActorOperation(ctx, "isolated")
			mustNil(t, err)
			// Моделируем явное освобождение оператором, не истечение TTL.
			mustNil(t, s.FinishActorOperation(ctx, old, true))
			var next ActorOperation
			if successor {
				next, err = s.AcquireActorOperation(ctx, "isolated", 1, "operator-successor", "human", "restart")
				mustNil(t, err)
			}
			mustNil(t, s.RecoverEphemeralChildAttestations(ctx))
			got, err := s.GetEphemeralChildAttestation(ctx, intent.ID)
			mustNil(t, err)
			if got.State != "unconfirmed" || !got.Replay || got.AttestedCommit != "" {
				t.Fatalf("recovery lost historical uncertainty: %+v", got)
			}
			current, err := s.CurrentActorOperation(ctx, "isolated")
			if successor {
				mustNil(t, err)
				if current != next {
					t.Fatalf("recovery changed successor: got %+v, want %+v", current, next)
				}
			} else if !errors.Is(err, ErrNotFound) {
				t.Fatalf("recovery recreated released hold: %+v %v", current, err)
			}
			before := databaseRows(t, s.db)
			mustNil(t, s.RecoverEphemeralChildAttestations(ctx))
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("repeated recovery changed retained history")
			}
			if err := s.RecordEphemeralChildAttestation(ctx, intent.ID, "attest-1", intent.Origin, intent.SourceCommit, childAllow); err == nil {
				t.Fatal("old proof accepted after explicit release or successor")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("old proof changed successor or history")
			}
			assertNoEphemeralExposure(t, s)
		})
	}
}
