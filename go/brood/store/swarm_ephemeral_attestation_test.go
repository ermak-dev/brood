package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Durable-аттестация workspace до регистрации эфемерного ребёнка (#292).
// SPEC: FULL-резерв даёт одно разрешение эффекта; повтор возвращает только историю.
// Неизвестный исход сохраняет точный lease без TTL; доказательство и освобождение
// коммитятся вместе после перепроверки текущих прав родителя.
// IMPLEMENTATION: Отдельный журнал намерения хранит закрытый workspace lease;
// переходы используют невложенные FULL-транзакции.
// RESULT: IMPLEMENTED
// STATUS: DONE
func ephemeralAttestationFixture(t *testing.T, ready bool) (*Store, EphemeralChildIntent) {
	t.Helper()
	s, intent := allocationFixture(t, 1)
	addEphemeralCapacity(t, s)
	_, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	if ready {
		setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
	}
	return s, intent
}

func assertNoEphemeralExposure(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"swarm_child_tasks", "actor_deliveries", "actor_delivery_attempts"} {
		var n int
		mustNil(t, s.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&n))
		if n != 0 {
			t.Fatalf("attestation exposed child payload via %s", table)
		}
	}
	var n int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM swarm_role_bindings WHERE actor_uid='isolated'`).Scan(&n))
	if n != 0 {
		t.Fatal("attestation rebound graph role")
	}
}

func TestEphemeralAttestationRequiresReadyActorAndFullReservation(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, false)
	before := databaseRows(t, s.db)
	if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow); err == nil {
		t.Fatal("provisioning actor obtained workspace lease")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("not-ready reservation wrote partial history")
	}
	setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 || a.Action != "attest_ephemeral" || a.Parent != intent.Parent || a.Executor.ActorUID != "isolated" || a.Executor.Incarnation != 1 || a.RequestID != intent.RequestID {
			t.Fatal("reservation lost durable authorization scope")
		}
		return nil
	}
	attempt, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", auth)
	mustNil(t, err)
	if attempt.Replay || attempt.State != "pending" || attempt.ActorUID != "isolated" || attempt.ActorIncarnation != 1 || attempt.IntentID != intent.ID || attempt.AttemptID != "attest-1" || attempt.Origin != intent.Origin || attempt.Commit != intent.SourceCommit || calls != 1 {
		t.Fatalf("wrong effect permission: %+v", attempt)
	}
	lease, err := s.CurrentActorOperation(ctx, "isolated")
	mustNil(t, err)
	if lease.Operation != "workspace_attest" || lease.Generation != 1 || lease.State != "inflight" {
		t.Fatal("reservation did not retain exact workspace hold")
	}
	before = databaseRows(t, s.db)
	repeated, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", auth)
	mustNil(t, err)
	if !repeated.Replay || repeated.AttemptID != attempt.AttemptID || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retry gave new effect permission or wrote history")
	}
	history, err := s.GetEphemeralChildAttestation(ctx, intent.ID)
	mustNil(t, err)
	if !reflect.DeepEqual(history, repeated) {
		t.Fatal("internal getter created authority")
	}
	if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-2", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatal("unresolved attempt was replaced")
	}
	if _, err := s.AcquireActorOperation(ctx, "isolated", 1, "restart", "human", "restart"); err == nil {
		t.Fatal("attestation failed to exclude workspace effects")
	}
	assertNoEphemeralExposure(t, s)
}

func TestEphemeralAttestationUnknownAndRecoveryNeverRearm(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "startup"}[recover], func(t *testing.T) {
			ctx := context.Background()
			s, intent := ephemeralAttestationFixture(t, true)
			_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
			mustNil(t, err)
			if recover {
				mustNil(t, s.RecoverEphemeralChildAttestations(ctx))
			} else {
				mustNil(t, s.UnconfirmEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow))
			}
			mustNil(t, s.RecoverEphemeralChildAttestations(ctx))
			got, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
			mustNil(t, err)
			if !got.Replay || got.State != "unconfirmed" || got.AttestedCommit != "" {
				t.Fatal("unknown outcome became effect permission or proof")
			}
			op, err := s.CurrentActorOperation(ctx, "isolated")
			mustNil(t, err)
			if op.State != "unconfirmed" {
				t.Fatal("unknown outcome lost workspace hold")
			}
			before := databaseRows(t, s.db)
			if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "new-after-timeout", childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("unknown attempt rearmed")
			}
			mustNil(t, s.RecordEphemeralChildAttestation(ctx, intent.ID, "attest-1", intent.Origin, intent.SourceCommit, childAllow))
			assertNoEphemeralExposure(t, s)
		})
	}
}

func TestEphemeralAttestationVerifiedProofReleasesOnlyExactLease(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, true)
	_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
	mustNil(t, err)
	parentStep := stepOf(t, s, 1)
	full := func(ctx context.Context, _ SwarmChildAuthorization) error {
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 {
			t.Fatal("proof transaction was not FULL")
		}
		return nil
	}
	mustNil(t, s.RecordEphemeralChildAttestation(ctx, intent.ID, "attest-1", intent.Origin, intent.SourceCommit, full))
	got, err := s.GetEphemeralChildAttestation(ctx, intent.ID)
	mustNil(t, err)
	if !got.Replay || got.State != "attested" || got.AttestedOrigin != intent.Origin || got.AttestedCommit != intent.SourceCommit {
		t.Fatal("verified source not retained")
	}
	if _, err := s.CurrentActorOperation(ctx, "isolated"); !errors.Is(err, ErrNotFound) {
		t.Fatal("verified proof retained workspace lease")
	}
	if !reflect.DeepEqual(parentStep, stepOf(t, s, 1)) {
		t.Fatal("ephemeral proof overwrote graph-step evidence")
	}
	before := databaseRows(t, s.db)
	mustNil(t, s.RecordEphemeralChildAttestation(ctx, intent.ID, "attest-1", intent.Origin, intent.SourceCommit, childAllow))
	mustNil(t, s.RecoverEphemeralChildAttestations(ctx))
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("proof retry or recovery rewrote settled history")
	}
	assertNoEphemeralExposure(t, s)
}

func TestEphemeralAttestationRefusesSpoofStaleScopeAndOwnerIntervention(t *testing.T) {
	for _, change := range []string{"nil", "denied", "origin", "commit", "attempt", "incarnation", "parent", "source", "stopped", "join", "owner", "lease"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, intent := ephemeralAttestationFixture(t, true)
			_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
			mustNil(t, err)
			origin, commit, id, auth := intent.Origin, intent.SourceCommit, "attest-1", SwarmChildAuthorize(childAllow)
			switch change {
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "origin":
				origin = "https://git.example.org/other/repo.git"
			case "commit":
				commit = strings.Repeat("f", 40)
			case "attempt":
				id = "spoof"
			case "incarnation":
				setActor(t, s, "isolated", "incarnation=2")
			case "parent":
				setActor(t, s, "a", "incarnation=2")
			case "source":
				// Имитируем несовместимый снимок отдельно от SQL-ограды источника:
				// Store обязан заново сверять source даже при таком повреждении.
				_, err = s.db.Exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://git.example.org/other/repo.git' WHERE swarm_id='one'`)
				mustNil(t, err)
			case "stopped":
				sw, e := s.GetSwarm(ctx, "one")
				mustNil(t, e)
				_, e = s.StopSwarm(ctx, "one", sw.OwnerID, allow)
				mustNil(t, e)
			case "join":
				_, err = s.db.Exec(`UPDATE swarm_child_joins SET state='aborted',revision=2,aborted_by='u',aborted_at='now'`)
				mustNil(t, err)
			case "owner":
				actor, e := s.GetManagedActor(ctx, "isolated")
				mustNil(t, e)
				sw, e := s.GetSwarm(ctx, "one")
				mustNil(t, e)
				_, e = s.AcquireOwnerActorOperation(ctx, actor, "owner-interrupt", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				mustNil(t, e)
			case "lease":
				op, e := s.CurrentActorOperation(ctx, "isolated")
				mustNil(t, e)
				mustNil(t, s.FinishActorOperation(ctx, op, true))
				_, e = s.AcquireActorOperation(ctx, "isolated", 1, "successor", "human", "restart")
				mustNil(t, e)
			}
			before := databaseRows(t, s.db)
			if err := s.RecordEphemeralChildAttestation(ctx, intent.ID, id, origin, commit, auth); err == nil {
				t.Fatal("invalid proof/current authority was accepted")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refusal partially attested source or released lease")
			}
			assertNoEphemeralExposure(t, s)
		})
	}
}

func TestEphemeralAttestationAtomicFailuresAndCancellation(t *testing.T) {
	for _, point := range []string{"reserve", "proof", "release", "release_after", "cancel"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			s, intent := ephemeralAttestationFixture(t, true)
			if point == "reserve" {
				_, err := s.db.Exec(`CREATE TRIGGER fail_attestation BEFORE INSERT ON ephemeral_child_attestations BEGIN SELECT RAISE(ABORT,'injected reservation failure'); END`)
				mustNil(t, err)
				before := databaseRows(t, s.db)
				got, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
				if err == nil || !reflect.DeepEqual(got, EphemeralChildAttestation{}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("reservation failure leaked lease or permission")
				}
				return
			}
			_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
			mustNil(t, err)
			if point == "proof" {
				_, err = s.db.Exec(`CREATE TRIGGER fail_proof BEFORE UPDATE ON ephemeral_child_attestations BEGIN SELECT RAISE(ABORT,'injected proof failure'); END`)
				mustNil(t, err)
			}
			if point == "release" {
				_, err = s.db.Exec(`CREATE TRIGGER fail_release BEFORE UPDATE ON coordination_claims WHEN NEW.slot IS NULL BEGIN SELECT RAISE(ABORT,'injected release failure'); END`)
				mustNil(t, err)
			}
			if point == "release_after" {
				_, err = s.db.Exec(`CREATE TRIGGER fail_release AFTER UPDATE ON coordination_claims WHEN NEW.slot IS NULL BEGIN SELECT RAISE(ABORT,'injected failure after both writes'); END`)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			auth := SwarmChildAuthorize(childAllow)
			if point == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				auth = func(context.Context, SwarmChildAuthorization) error { cancel(); return nil }
			}
			if err := s.RecordEphemeralChildAttestation(ctx, intent.ID, "attest-1", intent.Origin, intent.SourceCommit, auth); err == nil {
				t.Fatal("failed durable proof returned success")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("proof failure committed partial record or release")
			}
		})
	}
}

func TestEphemeralAttestationDeniesInvalidAndNestedReservations(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, true)
	before := databaseRows(t, s.db)
	for _, id := range []string{"", "bad\nidentity"} {
		if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, id, childAllow); err == nil {
			t.Fatal("invalid attempt was accepted")
		}
	}
	if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", nil); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatal("nil authorization granted permission")
	}
	for _, durable := range []bool{false, true} {
		run := s.WithTx
		if durable {
			run = s.WithDurableTx
		}
		mustNil(t, run(ctx, func(ctx context.Context) error {
			_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "attest-1", childAllow)
			if !errors.Is(err, ErrDurableTransactionNested) {
				t.Fatal("nested transaction returned effect permission")
			}
			return nil
		}))
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("denied reservation changed database")
	}
}
