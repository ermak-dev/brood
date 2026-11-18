package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// FEATURE: Регистрация эфемерного ребёнка только после retained proof (#292).
// SPEC: FULL commit, текущая authority, один логический слот и точный retry без записи.
// IMPLEMENTATION: Отдельный API, общий current scope и proof-bound SQL-конверсия.
// RESULT: IMPLEMENTED — поведенческий RED подтверждён до реализации; все проверки зелёные.
// STATUS: DONE
func registerEphemeral(t *testing.T, s *Store, ctx context.Context, id string, auth SwarmChildAuthorize) (SwarmChildTask, error) {
	t.Helper()
	return s.RegisterEphemeralChild(ctx, id, auth)
}

func attestForRegistration(t *testing.T, s *Store, intent EphemeralChildIntent) {
	t.Helper()
	ctx := context.Background()
	_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "proof", childAllow)
	mustNil(t, err)
	mustNil(t, s.RecordEphemeralChildAttestation(ctx, intent.ID, "proof", intent.Origin, intent.SourceCommit, childAllow))
}

func TestEphemeralRegistrationFullCommitAndExactRetry(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, true)
	assertNoEphemeralExposure(t, s)
	if _, err := registerEphemeral(t, s, ctx, intent.ID, childAllow); err == nil {
		t.Fatal("missing proof registered a child")
	}
	attestForRegistration(t, s, intent)
	assertNoEphemeralExposure(t, s)
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		executor := intent.Parent.Principal
		executor.ActorUID, executor.Incarnation = "isolated", 1
		if mode != 2 || a.Action != "register_ephemeral" || a.Caller != intent.Parent.Principal || a.Parent != intent.Parent || a.Executor != executor || a.RequestID != intent.RequestID || a.Payload != intent.Payload {
			t.Fatal("registration lost FULL mode or immutable scope")
		}
		return nil
	}
	child, err := registerEphemeral(t, s, ctx, intent.ID, auth)
	mustNil(t, err)
	if child.ID == "" || child.RequestID != intent.RequestID || child.Payload != intent.Payload || child.Executor.ActorUID != "isolated" || child.Executor.Incarnation != 1 || child.State != "pending" || child.Revision != 1 || child.Role != "" || child.Delivery != "" || child.Outcome != "" || child.Report != "" {
		t.Fatalf("wrong pending child: %+v", child)
	}
	join, err := s.GetSwarmChildJoin(ctx, intent.Parent, childAllow)
	mustNil(t, err)
	if len(join.Children) != 1 || join.Children[0] != child || join.Evaluation.Open != 1 {
		t.Fatal("child was not committed before return")
	}
	before := databaseRows(t, s.db)
	retry, err := registerEphemeral(t, s, ctx, intent.ID, auth)
	mustNil(t, err)
	if retry != child || calls != 2 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retry changed child or skipped reauthorization")
	}
	queued, err := s.QueuedSwarmChildren(ctx)
	mustNil(t, err)
	if len(queued) != 0 {
		t.Fatal("ephemeral class entered role dispatcher")
	}
	if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "proof", childAllow); err == nil {
		t.Fatal("registration weakened pre-registration attestation fence")
	}
}

func TestEphemeralRegistrationOrdinaryReserveCannotBypass(t *testing.T) {
	for _, proof := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-proof", true: "attested"}[proof], func(t *testing.T) {
			s, intent := allocationFixture(t, 3)
			addEphemeralCapacity(t, s)
			_, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
			if proof {
				attestForRegistration(t, s, intent)
			}
			executor := intent.Parent.Principal
			executor.ActorUID = "isolated"
			before := databaseRows(t, s.db)
			for _, key := range []string{intent.RequestID, "spare-key"} {
				if _, err := s.ReserveSwarmChild(context.Background(), intent.Parent, key, intent.Payload, executor, childAllow); err == nil {
					t.Fatal("ordinary reserve bypassed proof-bound registration")
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("bypass refusal changed rows")
			}
		})
	}
}

func TestEphemeralRegistrationSQLConversionAndSpoofFences(t *testing.T) {
	for _, change := range []string{"exact", "missing-proof", "pending-proof", "unknown-proof", "key", "actor", "incarnation", "payload", "role", "swarm", "parent"} {
		t.Run(change, func(t *testing.T) {
			s, intent := allocationFixture(t, 3)
			addEphemeralCapacity(t, s)
			_, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
			_, err = s.EnqueueEphemeralChild(context.Background(), intent.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 3, Remaining: "retain"}, "stolen", "Other job payload", "pool", childAllow)
			mustNil(t, err)
			if change != "missing-proof" {
				_, err := s.ReserveEphemeralChildAttestation(context.Background(), intent.ID, "proof", childAllow)
				mustNil(t, err)
				if change == "unknown-proof" {
					mustNil(t, s.UnconfirmEphemeralChildAttestation(context.Background(), intent.ID, "proof", childAllow))
				} else if change != "pending-proof" {
					mustNil(t, s.RecordEphemeralChildAttestation(context.Background(), intent.ID, "proof", intent.Origin, intent.SourceCommit, childAllow))
				}
			}
			key, payload, uid, role, delivery, swarm, seq, inc := intent.RequestID, intent.Payload, "isolated", "", "", intent.Parent.Principal.SwarmID, intent.Parent.Sequence, int64(1)
			switch change {
			case "key":
				key, payload = "stolen", "Other job payload"
			case "actor":
				uid = "b"
			case "incarnation":
				inc = 2
			case "payload":
				payload = "spoof"
			case "role":
				role, delivery = "worker", "queued"
			case "swarm":
				swarm = "s"
			case "parent":
				seq++
			}
			before := databaseRows(t, s.db)
			_, err = s.db.Exec(`INSERT INTO swarm_child_tasks(id,swarm_id,parent_sequence,request_id,payload,actor_uid,actor_incarnation,state,revision,created_at,role,delivery) VALUES('sql-child',?,?,?,?,?,?,'pending',1,'now',?,?)`, swarm, seq, key, payload, uid, inc, role, delivery)
			if change == "exact" {
				mustNil(t, err)
			} else if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("SQL accepted spoof or changed rows")
			}
		})
	}
}

func TestEphemeralRegistrationRefusalsAreReadOnly(t *testing.T) {
	for _, change := range []string{"nil", "denied", "missing", "pending", "unknown", "foreign", "actor", "parent", "not-ready", "stopped", "join", "pool", "source", "owner", "owner-settled", "effect", "unknown-effect"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, intent := ephemeralAttestationFixture(t, true)
			if change == "pending" || change == "unknown" {
				_, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "proof", childAllow)
				mustNil(t, err)
				if change == "unknown" {
					mustNil(t, s.UnconfirmEphemeralChildAttestation(ctx, intent.ID, "proof", childAllow))
				}
			} else if change != "missing" {
				attestForRegistration(t, s, intent)
			}
			id, auth := intent.ID, SwarmChildAuthorize(childAllow)
			switch change {
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "foreign":
				id = "foreign-intent"
			case "actor":
				setActor(t, s, "isolated", "incarnation=2")
			case "parent":
				setActor(t, s, "a", "incarnation=2")
			case "not-ready":
				setActor(t, s, "isolated", "phase='stopped'")
			case "stopped":
				sw, err := s.GetSwarm(ctx, "one")
				mustNil(t, err)
				_, err = s.StopSwarm(ctx, "one", sw.OwnerID, allow)
				mustNil(t, err)
			case "join":
				_, err := s.db.Exec(`UPDATE swarm_child_joins SET state='aborted',revision=2,aborted_by='u',aborted_at='now'`)
				mustNil(t, err)
			case "pool":
				pool, err := s.GetManagedPool(ctx, "pool")
				mustNil(t, err)
				pool.State = "draining"
				_, err = s.UpdateManagedPool(ctx, pool, pool.Revision)
				mustNil(t, err)
			case "source":
				_, err := s.db.Exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git' WHERE swarm_id='one'`)
				mustNil(t, err)
			case "owner", "owner-settled":
				a, err := s.GetManagedActor(ctx, "isolated")
				mustNil(t, err)
				sw, err := s.GetSwarm(ctx, "one")
				mustNil(t, err)
				op, err := s.AcquireOwnerActorOperation(ctx, a, "intervene", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				mustNil(t, err)
				if change == "owner-settled" {
					mustNil(t, s.FinishActorOperation(ctx, op, true))
				}
			case "effect", "unknown-effect":
				op, err := s.AcquireActorOperation(ctx, "isolated", 1, "effect", "trusted", "workspace_attest")
				mustNil(t, err)
				if change == "unknown-effect" {
					mustNil(t, s.FinishActorOperation(ctx, op, false))
				}
			}
			before := databaseRows(t, s.db)
			got, err := registerEphemeral(t, s, ctx, id, auth)
			if err == nil || got != (SwarmChildTask{}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("invalid registration returned child or mutated state")
			}
		})
	}
}

func TestEphemeralRegistrationAtomicityNestingCancellationAndConcurrency(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, true)
	attestForRegistration(t, s, intent)
	before := databaseRows(t, s.db)
	for _, run := range []func(context.Context, func(context.Context) error) error{s.WithTx, s.WithDurableTx} {
		mustNil(t, run(ctx, func(ctx context.Context) error {
			_, err := registerEphemeral(t, s, ctx, intent.ID, childAllow)
			if !errors.Is(err, ErrDurableTransactionNested) {
				t.Fatal("nested registration did not deny before commit")
			}
			return nil
		}))
	}
	canceled, cancel := context.WithCancel(ctx)
	_, err := registerEphemeral(t, s, canceled, intent.ID, func(context.Context, SwarmChildAuthorization) error { cancel(); return nil })
	if err == nil {
		t.Fatal("cancelled registration succeeded")
	}
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error { return nil }))
	_, err = s.db.Exec(`CREATE TRIGGER fail_registration AFTER INSERT ON swarm_child_tasks BEGIN SELECT RAISE(ABORT,'injected registration failure'); END`)
	mustNil(t, err)
	if _, err := registerEphemeral(t, s, ctx, intent.ID, childAllow); err == nil {
		t.Fatal("injected write failure succeeded")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("refusal leaked partial registration")
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_registration`)
	mustNil(t, err)
	var wg sync.WaitGroup
	results := make(chan SwarmChildTask, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			child, err := registerEphemeral(t, s, ctx, intent.ID, childAllow)
			results <- child
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	var first SwarmChildTask
	for child := range results {
		if first.ID == "" {
			first = child
		} else if first != child {
			t.Fatal("concurrent retry returned a different child")
		}
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM swarm_child_tasks`).Scan(&count))
	if count != 1 {
		t.Fatal("concurrent registrations did not produce exactly one child")
	}
}
