package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: Перепроверка pending аттестации перед открытием корневого RPC (#292).
// SPEC: Только текущие parent/source/allocation и точный inflight lease;
// чтение не пишет историю, не выдаёт нового разрешения и не возобновляет эффект.
// IMPLEMENTATION: Проверка текущего scope и точного inflight lease в read-транзакции.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestCheckEphemeralAttestationRequiresCurrentInflightAttempt(t *testing.T) {
	for _, scenario := range []string{"pending", "nil", "denied", "attempt", "unknown", "recovered", "attested", "released", "successor", "parent", "owner"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, intent := ephemeralAttestationFixture(t, true)
			_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
			mustNil(t, err)
			id, auth := "attest-1", SwarmChildAuthorize(childAllow)
			switch scenario {
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "attempt":
				id = "other"
			case "unknown":
				mustNil(t, s.UnconfirmEphemeralChildAttestation(ctx, intent.ID, id, childAllow))
			case "recovered":
				mustNil(t, s.RecoverActorOperations(ctx))
			case "attested":
				mustNil(t, s.RecordEphemeralChildAttestation(ctx, intent.ID, id, intent.Origin, intent.SourceCommit, childAllow))
			case "released", "successor":
				lease, err := s.CurrentActorOperation(ctx, "isolated")
				mustNil(t, err)
				mustNil(t, s.FinishActorOperation(ctx, lease, true))
				if scenario == "successor" {
					_, err = s.AcquireActorOperation(ctx, "isolated", 1, "next", "human", "restart")
					mustNil(t, err)
				}
			case "parent":
				setActor(t, s, "a", "incarnation=2")
			case "owner":
				actor, err := s.GetManagedActor(ctx, "isolated")
				mustNil(t, err)
				sw, err := s.GetSwarm(ctx, "one")
				mustNil(t, err)
				_, err = s.AcquireOwnerActorOperation(ctx, actor, "owner-interrupt", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			err = s.CheckEphemeralChildAttestation(ctx, intent.ID, id, auth)
			if (err == nil) != (scenario == "pending") {
				t.Fatalf("wrong pre-RPC check for %s: %v", scenario, err)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("pre-RPC check mutated history or lease")
			}
		})
	}
}
