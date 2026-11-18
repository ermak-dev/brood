package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Startup сохраняет неопределённость эфемерной аттестации (#292).
// SPEC: До запуска диспетчеров pending становится unconfirmed без нового
// эффекта; явно освобождённый lease не воссоздаётся, преемник не теряется.
// IMPLEMENTATION: Recovery вызывается до сборки диспетчеров в server.New.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralAttestationServerStartupRetainsUncertainty(t *testing.T) {
	for _, scenario := range []string{"pending", "released", "successor"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, reservation := ephemeralCapacityFixture(t)
			ctx := context.Background()
			allocation, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation)
			if err != nil {
				t.Fatal(err)
			}
			// Готовность провайдера моделируется только в изолированной test DB.
			raw, err := sql.Open("sqlite", d.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = raw.Exec(`UPDATE managed_actors SET phase='running',stage='done',operation='start' WHERE uid=?`, allocation.ActorUID)
			closeErr := raw.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("prepare ready fixture: %v %v", err, closeErr)
			}
			_, err = d.srv.store.ReserveEphemeralChildAttestation(ctx, intent.ID, "startup-attest", d.srv.managed.childAuthority)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := d.srv.store.CurrentActorOperation(ctx, allocation.ActorUID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "pending" {
				if err := d.srv.store.FinishActorOperation(ctx, lease, true); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "successor" {
				lease, err = d.srv.store.AcquireActorOperation(ctx, allocation.ActorUID, allocation.ActorIncarnation, "operator-successor", "human", "restart")
				if err != nil {
					t.Fatal(err)
				}
			}
			d.http.Close()
			if err := d.srv.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(ctx, Config{DBPath: d.dbPath}, Options{argon2: cheapArgon2})
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			got, err := restarted.store.GetEphemeralChildAttestation(ctx, intent.ID)
			if err != nil || got.State != "unconfirmed" || !got.Replay || got.AttemptID != "startup-attest" || got.AttestedCommit != "" {
				t.Fatalf("startup did not retain uncertainty: %+v %v", got, err)
			}
			current, err := restarted.store.CurrentActorOperation(ctx, allocation.ActorUID)
			if scenario == "released" {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("startup recreated released hold: %+v %v", current, err)
				}
			} else {
				lease.State = "unconfirmed"
				if err != nil || current != lease {
					t.Fatalf("startup replaced held operation: %+v %v", current, err)
				}
			}
			for _, table := range []string{"swarm_child_tasks", "actor_deliveries"} {
				db, err := sql.Open("sqlite", d.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				var count int
				err = db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count)
				db.Close()
				if err != nil || count != 0 {
					t.Fatalf("startup exposed work through %s: %d %v", table, count, err)
				}
			}
		})
	}
}
