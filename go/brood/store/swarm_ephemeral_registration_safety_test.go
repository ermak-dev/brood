package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func registrationFixture(t *testing.T, total int) (*Store, EphemeralChildIntent) {
	t.Helper()
	s, intent := allocationFixture(t, total)
	addEphemeralCapacity(t, s)
	_, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
	attestForRegistration(t, s, intent)
	return s, intent
}

func TestEphemeralRegistrationMixedMembershipUsesOneSlot(t *testing.T) {
	for _, ordinaryFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "queue-first", true: "ordinary-first"}[ordinaryFirst], func(t *testing.T) {
			ctx := context.Background()
			s, intent := registrationFixture(t, 3)
			_, err := s.RegisterEphemeralChild(ctx, intent.ID, childAllow)
			mustNil(t, err)
			policy := SwarmChildJoinPolicy{Mode: "all", Total: 3, Remaining: "retain"}
			executor := intent.Parent.Principal
			executor.ActorUID = "b"
			enqueue := func() {
				_, err := s.EnqueueEphemeralChild(ctx, intent.Parent, policy, "queued-second", "Second isolated task", "pool", childAllow)
				mustNil(t, err)
			}
			ordinary := func() {
				_, err := s.ReserveSwarmChild(ctx, intent.Parent, "ordinary-third", "Third plain task", executor, childAllow)
				mustNil(t, err)
			}
			if ordinaryFirst {
				ordinary()
				enqueue()
			} else {
				enqueue()
				ordinary()
			}
			join, err := s.GetSwarmChildJoin(ctx, intent.Parent, childAllow)
			mustNil(t, err)
			if len(join.Children) != 2 || join.Evaluation.Open != 3 {
				t.Fatal("mixed membership snapshot lost pending/absent obligations")
			}
			before := databaseRows(t, s.db)
			if _, err := s.EnqueueEphemeralChild(ctx, intent.Parent, policy, "overflow-queue", "Overflow", "pool", childAllow); err == nil {
				t.Fatal("mixed full join accepted another queued slot")
			}
			if _, err := s.ReserveSwarmChild(ctx, intent.Parent, "overflow-plain", "Overflow", executor, childAllow); err == nil {
				t.Fatal("ordinary reserve stole a queued slot")
			}
			for _, query := range []string{
				`INSERT INTO swarm_child_tasks(id,swarm_id,parent_sequence,request_id,payload,actor_uid,actor_incarnation,state,revision,created_at) VALUES('overflow','one',1,'overflow','Overflow','b',1,'pending',1,'now')`,
				`INSERT INTO ephemeral_child_intents SELECT 'overflow',swarm_id,parent_sequence,pass,project_id,atespace_uid,parent_uid,parent_incarnation,parent_request_id,'overflow','Overflow',pool_uid,source_fingerprint,template,source_commit,origin,created_at FROM ephemeral_child_intents LIMIT 1`,
				`INSERT INTO swarm_child_tasks(id,swarm_id,parent_sequence,request_id,payload,actor_uid,actor_incarnation,state,revision,created_at) VALUES('steal','one',1,'queued-second','Second isolated task','b',1,'pending',1,'now')`,
			} {
				if _, err := s.db.Exec(query); err == nil {
					t.Fatal("SQL stole queued membership or exceeded join total")
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("capacity refusal changed rows")
			}
		})
	}
}

func TestEphemeralRegistrationConvertsFullyQueuedJoin(t *testing.T) {
	ctx := context.Background()
	s, first := registrationFixture(t, 2)
	second, err := s.EnqueueEphemeralChild(ctx, first.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second", "Second isolated task", "pool", childAllow)
	mustNil(t, err)
	addEphemeralCapacity(t, s)
	_, err = s.AllocateEphemeralChild(ctx, second.ID, ephemeralReservation("isolated-second", 304), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	setActor(t, s, "isolated-second", "phase='running',stage='done',operation='start'")
	attestForRegistration(t, s, second)
	for _, intent := range []EphemeralChildIntent{first, second} {
		_, err := s.RegisterEphemeralChild(ctx, intent.ID, childAllow)
		mustNil(t, err)
	}
	join, err := s.GetSwarmChildJoin(ctx, first.Parent, childAllow)
	mustNil(t, err)
	if len(join.Children) != 2 || join.Evaluation.Open != 2 {
		t.Fatal("full queue failed to convert each reserved slot exactly once")
	}
}

func TestEphemeralRegistrationRetryStillRequiresCurrentScope(t *testing.T) {
	for _, change := range []string{"nil", "denied", "actor", "parent", "interrupted", "stopped", "effect", "nested", "durable-nested", "cancelled", "cancel-in-callback", "already-cancelled"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, intent := registrationFixture(t, 1)
			child, err := s.RegisterEphemeralChild(ctx, intent.ID, childAllow)
			mustNil(t, err)
			auth := SwarmChildAuthorize(childAllow)
			switch change {
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "actor":
				setActor(t, s, "isolated", "incarnation=2")
			case "parent":
				setActor(t, s, "a", "incarnation=2")
			case "interrupted":
				a, err := s.GetManagedActor(ctx, "a")
				mustNil(t, err)
				sw, err := s.GetSwarm(ctx, "one")
				mustNil(t, err)
				_, err = s.AcquireOwnerActorOperation(ctx, a, "parent-intervention", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				mustNil(t, err)
			case "stopped":
				sw, err := s.GetSwarm(ctx, "one")
				mustNil(t, err)
				_, err = s.StopSwarm(ctx, "one", sw.OwnerID, allow)
				mustNil(t, err)
			case "effect":
				_, err := s.AcquireOwnerActorOperation(ctx, ManagedActor{UID: "isolated", Incarnation: 1, SwarmID: "one", AtespaceUID: intent.Parent.Principal.AtespaceUID}, "owner-effect", "human", "session_terminal", "u", func(context.Context) error { return nil })
				mustNil(t, err)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "cancel-in-callback":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				auth = func(context.Context, SwarmChildAuthorization) error { cancel(); return nil }
			case "already-cancelled":
				child, err = s.CancelSwarmChild(ctx, intent.Parent, intent.RequestID, child.Revision, childAllow)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			if change == "nested" || change == "durable-nested" {
				run := s.WithTx
				if change == "durable-nested" {
					run = s.WithDurableTx
				}
				mustNil(t, run(ctx, func(ctx context.Context) error {
					_, err := s.RegisterEphemeralChild(ctx, intent.ID, auth)
					if !errors.Is(err, ErrDurableTransactionNested) {
						t.Fatal("exact retry escaped outer transaction")
					}
					return nil
				}))
			} else {
				got, err := s.RegisterEphemeralChild(ctx, intent.ID, auth)
				if change == "already-cancelled" {
					mustNil(t, err)
					if got != child {
						t.Fatal("retry replaced cancelled child")
					}
				} else if err == nil || got != (SwarmChildTask{}) {
					t.Fatal("stale retry returned a child")
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("retry changed retained state")
			}
		})
	}
}

func TestEphemeralRegistrationRequiresCurrentActorBindings(t *testing.T) {
	for _, mutation := range []string{"actor_class='persistent'", "parent_uid='b'", "pool_uid='foreign'", "template='{}'", "source_fingerprint='foreign'", "origin='https://other.example/repo.git'", "swarm_id='two'", "atespace_uid='foreign'"} {
		t.Run(mutation, func(t *testing.T) {
			s, intent := registrationFixture(t, 1)
			if mutation == "pool_uid='foreign'" {
				pool, err := s.GetManagedPool(context.Background(), "pool")
				mustNil(t, err)
				pool.UID, pool.Name = "foreign", "foreign"
				_, err = s.CreateManagedPool(context.Background(), pool)
				mustNil(t, err)
			}
			// Снимок намеренно повреждён за SQL-оградой класса; scope всё равно проверяется.
			_, err := s.db.Exec(`DROP TRIGGER managed_actor_class_update_guard; UPDATE managed_actors SET ` + mutation + ` WHERE uid='isolated'`)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			if _, err := s.RegisterEphemeralChild(context.Background(), intent.ID, childAllow); err == nil {
				t.Fatal("registration accepted changed actor binding")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("binding refusal changed history")
			}
		})
	}
}

func TestEphemeralRegistrationRejectsCorruptOrForeignRetainedProof(t *testing.T) {
	for _, mutation := range []string{"actor_uid='b'", "actor_incarnation=2", "origin='https://other.example/repo.git',attested_origin='https://other.example/repo.git'", "source_commit='foreign',attested_commit='foreign'"} {
		t.Run(mutation, func(t *testing.T) {
			s, intent := registrationFixture(t, 1)
			// Повреждённая история имитируется отдельно от SQL-ограды; API не доверяет ей.
			_, err := s.db.Exec(`DROP TRIGGER ephemeral_child_attestation_update_guard; PRAGMA ignore_check_constraints=ON; UPDATE ephemeral_child_attestations SET ` + mutation + `; PRAGMA ignore_check_constraints=OFF`)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			if _, err := s.RegisterEphemeralChild(context.Background(), intent.ID, childAllow); err == nil {
				t.Fatal("foreign retained proof registered a child")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("foreign proof refusal changed history")
			}
		})
	}
}
