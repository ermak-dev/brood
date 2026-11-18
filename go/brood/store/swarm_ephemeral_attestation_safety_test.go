package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// FEATURE: Ограждения аттестации при сбое и конкуренции (#292).
// SPEC: Одно свежее разрешение, неизменяемая история, без эффектов recovery/replay.
// IMPLEMENTATION: Реальные транзакции, переоткрытие и SQL-ограждения журнала.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralAttestationConcurrentReservationsGrantOnePermission(t *testing.T) {
	s, intent := ephemeralAttestationFixture(t, true)
	var wg sync.WaitGroup
	type result struct {
		value EphemeralChildAttestation
		err   error
	}
	results := make(chan result, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := s.ReserveEphemeralChildAttestation(context.Background(), intent.ID, "attest-1", childAllow)
			results <- result{value, err}
		}()
	}
	wg.Wait()
	close(results)
	fresh := 0
	for got := range results {
		mustNil(t, got.err)
		if !got.value.Replay {
			fresh++
		}
		if got.value.AttemptID != "attest-1" || got.value.State != "pending" {
			t.Fatal("concurrent reservation changed retained attempt")
		}
	}
	if fresh != 1 {
		t.Fatalf("got %d effect permissions, want exactly one", fresh)
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM ephemeral_child_attestations`).Scan(&count))
	if count != 1 {
		t.Fatal("concurrent reservation duplicated ledger history")
	}
	assertNoEphemeralExposure(t, s)
}

func TestEphemeralAttestationReopenAndRecoveryKeepRevokedHold(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, true)
	_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	_, err = s.StopSwarm(ctx, "one", sw.OwnerID, allow)
	mustNil(t, err)
	setActor(t, s, "isolated", "incarnation=2")
	var path string
	mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	before, err := reopened.GetEphemeralChildAttestation(ctx, intent.ID)
	mustNil(t, err)
	if before.State != "pending" || !before.Replay {
		t.Fatal("Open lost or rearmed retained attempt")
	}
	// Оба recovery допустимы в одном startup; общий recovery не знает ledger.
	mustNil(t, reopened.RecoverActorOperations(ctx))
	mustNil(t, reopened.RecoverEphemeralChildAttestations(ctx))
	got, err := reopened.GetEphemeralChildAttestation(ctx, intent.ID)
	mustNil(t, err)
	op, err := reopened.CurrentActorOperation(ctx, "isolated")
	mustNil(t, err)
	if got.State != "unconfirmed" || !got.Replay || op.State != "unconfirmed" || op.Generation != 1 {
		t.Fatal("startup dropped old-incarnation hold after parent stop")
	}
	rows := databaseRows(t, reopened.db)
	if err := reopened.RecordEphemeralChildAttestation(ctx, intent.ID, "attest-1", intent.Origin, intent.SourceCommit, childAllow); err == nil {
		t.Fatal("revoked allocation accepted late proof")
	}
	if !reflect.DeepEqual(rows, databaseRows(t, reopened.db)) {
		t.Fatal("late refusal released old hold")
	}
	assertNoEphemeralExposure(t, reopened)
}

func TestEphemeralAttestationHistoryCannotBeRewrittenOrRearmed(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, true)
	_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
	mustNil(t, err)
	mustNil(t, s.RecordEphemeralChildAttestation(ctx, intent.ID, "attest-1", intent.Origin, intent.SourceCommit, childAllow))
	before := databaseRows(t, s.db)
	for _, query := range []string{
		`DELETE FROM ephemeral_child_attestations WHERE intent_id=?`,
		`INSERT OR REPLACE INTO ephemeral_child_attestations SELECT * FROM ephemeral_child_attestations WHERE intent_id=?`,
		`UPDATE ephemeral_child_attestations SET state='pending',attested_origin='',attested_commit='' WHERE intent_id=?`,
		`UPDATE ephemeral_child_attestations SET attempt_id='replacement' WHERE intent_id=?`,
		`UPDATE ephemeral_child_attestations SET lease='{}' WHERE intent_id=?`,
		`UPDATE ephemeral_child_attestations SET attested_commit='forged' WHERE intent_id=?`,
	} {
		if _, err := s.db.Exec(query, intent.ID); err == nil {
			t.Fatalf("ledger history mutation succeeded: %s", query)
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("refused mutation changed retained history")
		}
	}
	repeated, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
	mustNil(t, err)
	if !repeated.Replay || repeated.State != "attested" {
		t.Fatal("settled retry granted effect permission")
	}
	if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-2", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatal("settled intent rearmed")
	}
	if err := s.UnconfirmEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatal("settled proof became uncertain")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("settled history changed on retry")
	}
}
