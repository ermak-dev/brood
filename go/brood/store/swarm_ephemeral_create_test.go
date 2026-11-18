package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// FEATURE: Одно durable-разрешение первого физического create для allocation (#292).
// SPEC: Новый allocation атомарно получает queued grant; прежняя история не разрешает Create.
// IMPLEMENTATION: Отдельный ledger, FULL reserve, read-only check и однонаправленные SQL-ограды.
// RESULT: IMPLEMENTED — queued сохраняется до первого reserve, reserved никогда не переигрывается.
// STATUS: DONE
func createGrantFixture(t *testing.T) (*Store, EphemeralChildIntent) {
	t.Helper()
	s, intent := allocationFixture(t, 1)
	addEphemeralCapacity(t, s)
	_, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	return s, intent
}

func TestEphemeralCreateOneFreshPermissionAndReadOnlyRetry(t *testing.T) {
	ctx := context.Background()
	s, intent := createGrantFixture(t)
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 || a.Action != "create_ephemeral" || a.Parent != intent.Parent || a.Caller != intent.Parent.Principal || a.Executor.ActorUID != "isolated" || a.Executor.Incarnation != 1 || !sameChildScope(a.Executor, intent.Parent.Principal) || a.RequestID != intent.RequestID || a.Payload != intent.Payload {
			t.Fatal("create authority lacked FULL exact scope")
		}
		return nil
	}
	before := databaseRows(t, s.db)
	if err := s.CheckEphemeralChildCreate(ctx, intent.ID, childAllow); err == nil {
		t.Fatal("queued grant passed reserved check")
	}
	history, err := s.GetEphemeralChildCreateGrant(ctx, intent.ID)
	mustNil(t, err)
	if !history.Replay || history.State != "queued" || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("history read granted permission or wrote rows")
	}
	fresh, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, auth)
	mustNil(t, err)
	if fresh.Replay || fresh.State != "reserved" || fresh.ActorUID != "isolated" || fresh.ActorIncarnation != 1 || fresh.CreateOperationID != "create-isolated" || fresh.ReservedAt == "" {
		t.Fatal("fresh reservation did not grant exact one create")
	}
	before = databaseRows(t, s.db)
	retry, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, auth)
	mustNil(t, err)
	if !retry.Replay || calls != 2 {
		t.Fatal("retry was not reauthorized history")
	}
	retry.Replay = false
	if retry != fresh {
		t.Fatal("retry changed original grant")
	}
	mustNil(t, s.CheckEphemeralChildCreate(ctx, intent.ID, childAllow))
	_, err = s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("isolated", 303), childAllow, nil)
	mustNil(t, err)
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("check/retry/allocation retry rearmed history")
	}
	// Bootstrap source остаётся доступным без ready или нового grant.
	got, err := s.GetEphemeralChildIntentForActor(ctx, "isolated", 1)
	mustNil(t, err)
	if got.ID != intent.ID {
		t.Fatal("cold source lookup lost allocation")
	}
}

func TestEphemeralCreateConcurrentReservations(t *testing.T) {
	s, intent := createGrantFixture(t)
	type result struct {
		value EphemeralChildCreateGrant
		err   error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.ReserveEphemeralChildCreate(context.Background(), intent.ID, childAllow)
			results <- result{v, err}
		}()
	}
	wg.Wait()
	close(results)
	fresh := 0
	for r := range results {
		mustNil(t, r.err)
		if !r.value.Replay {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("got %d fresh permissions, want one", fresh)
	}
}

func TestEphemeralCreateIgnoredWritesNeverGrantPermission(t *testing.T) {
	for _, point := range []string{"allocate", "reserve"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			s, intent := allocationFixture(t, 1)
			addEphemeralCapacity(t, s)
			if point == "reserve" {
				_, err := s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
				mustNil(t, err)
			}
			statement := `CREATE TRIGGER ignore_grant BEFORE INSERT ON ephemeral_child_create_grants BEGIN SELECT RAISE(IGNORE); END`
			if point == "reserve" {
				statement = `CREATE TRIGGER ignore_grant BEFORE UPDATE ON ephemeral_child_create_grants BEGIN SELECT RAISE(IGNORE); END`
			}
			_, err := s.db.Exec(statement)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			if point == "allocate" {
				v, err := s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
				if err == nil || v != (EphemeralChildAllocation{}) {
					t.Fatal("allocation committed without queued grant")
				}
			} else {
				v, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
				if err == nil || v != (EphemeralChildCreateGrant{}) {
					t.Fatal("ignored transition granted fresh Create")
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("ignored grant write left partial history")
			}
		})
	}
}

func TestEphemeralCreateFailuresRollbackAndRejectNesting(t *testing.T) {
	for _, failure := range []string{"nil-authority", "denied", "cancel-before", "cancel-callback", "tx", "durable-tx", "update", "after-update", "allocation-insert"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			s, intent := allocationFixture(t, 1)
			addEphemeralCapacity(t, s)
			if failure == "allocation-insert" {
				_, err := s.db.Exec(`CREATE TRIGGER test_grant_failure BEFORE INSERT ON ephemeral_child_create_grants BEGIN SELECT RAISE(ABORT,'synthetic grant failure'); END`)
				mustNil(t, err)
				before := databaseRows(t, s.db)
				v, err := s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
				if err == nil || v != (EphemeralChildAllocation{}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("grant failure committed actor/allocation")
				}
				return
			}
			_, err := s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			auth := SwarmChildAuthorize(childAllow)
			var want error
			switch failure {
			case "nil-authority":
				auth = nil
				want = ErrSwarmChildDenied
			case "denied":
				want = errors.New("authority revoked")
				auth = func(context.Context, SwarmChildAuthorization) error { return want }
			case "cancel-before", "cancel-callback":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				want = context.Canceled
				if failure == "cancel-before" {
					cancel()
				} else {
					auth = func(context.Context, SwarmChildAuthorization) error { cancel(); return nil }
				}
			case "after-update":
				_, err := s.db.Exec(`CREATE TRIGGER test_grant_failure AFTER UPDATE ON ephemeral_child_create_grants BEGIN SELECT RAISE(ABORT,'synthetic failure after reserve'); END`)
				mustNil(t, err)
			case "update":
				_, err := s.db.Exec(`CREATE TRIGGER test_grant_failure BEFORE UPDATE ON ephemeral_child_create_grants BEGIN SELECT RAISE(ABORT,'synthetic grant failure'); END`)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			if failure == "tx" || failure == "durable-tx" {
				run := s.WithTx
				if failure == "durable-tx" {
					run = s.WithDurableTx
				}
				mustNil(t, run(ctx, func(ctx context.Context) error {
					_, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, auth)
					if !errors.Is(err, ErrDurableTransactionNested) {
						t.Fatal("nested reservation returned effect permission")
					}
					return nil
				}))
			} else {
				v, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, auth)
				if err == nil || v != (EphemeralChildCreateGrant{}) || (want != nil && !errors.Is(err, want)) {
					t.Fatalf("failed reservation returned permission: %+v %v", v, err)
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refusal left partial grant")
			}
		})
	}
}

func TestEphemeralCreateReopenPreservesQueuedAndReserved(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "reserved"}[reserved], func(t *testing.T) {
			ctx := context.Background()
			s, intent := createGrantFixture(t)
			if reserved {
				_, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
				mustNil(t, err)
			}
			var path string
			mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
			mustNil(t, s.Close())
			reopened, err := Open(ctx, path)
			mustNil(t, err)
			defer reopened.Close()
			before := databaseRows(t, reopened.db)
			v, err := reopened.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
			mustNil(t, err)
			if v.Replay != reserved {
				t.Fatal("restart lost unissued grant or repeated reserved grant")
			}
			if reserved && !reflect.DeepEqual(before, databaseRows(t, reopened.db)) {
				t.Fatal("restart rearmed reserved grant")
			}
		})
	}
}

func TestEphemeralCreateCurrentScopeRefusals(t *testing.T) {
	changes := []string{"incarnation", "parent-incarnation", "parent-request", "parent-state", "new-pass", "pass-source", "project", "swarm-namespace", "interrupted", "class", "role", "parent", "swarm", "namespace", "origin", "template", "fingerprint", "cluster", "operation-id", "operation", "stage", "phase", "desired", "upid", "nonce", "ctid", "resources", "source", "pool", "pool-template", "pool-fingerprint", "pool-cluster", "stopped", "join", "owner", "owner-settled", "effect", "unknown-effect", "nil", "denied"}
	for _, change := range changes {
		for _, reserved := range []bool{false, true} {
			t.Run(change+map[bool]string{false: "/queued", true: "/reserved"}[reserved], func(t *testing.T) {
				ctx := context.Background()
				s, intent := createGrantFixture(t)
				if reserved {
					_, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
					mustNil(t, err)
				}
				exec := func(query string, args ...any) { t.Helper(); _, err := s.db.Exec(query, args...); mustNil(t, err) }
				auth := SwarmChildAuthorize(childAllow)
				switch change {
				case "incarnation":
					setActor(t, s, "isolated", "incarnation=2")
				case "parent-incarnation":
					setActor(t, s, "a", "incarnation=2")
				case "parent-request":
					exec(`DROP TRIGGER swarm_steps_child_identity; UPDATE swarm_steps SET request_id='other' WHERE swarm_id='one'`)
				case "parent-state":
					exec(`UPDATE swarm_steps SET state='pending' WHERE swarm_id='one'`)
				case "new-pass":
					exec(`DROP TRIGGER swarm_steps_child_insert; INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,goal,source_commit) SELECT swarm_id,2,node_id,'pending','Next pass',source_commit FROM swarm_steps WHERE swarm_id='one' AND sequence=1`)
				case "pass-source":
					exec(`DROP TRIGGER swarm_steps_pass_fence; DROP TRIGGER swarm_steps_child_identity; UPDATE swarm_steps SET source_commit='ffffffffffffffffffffffffffffffffffffffff',attested_commit='ffffffffffffffffffffffffffffffffffffffff' WHERE swarm_id='one'`)
				case "project":
					seedProject(t, s, "other-project", "Other")
					exec(`UPDATE swarms SET project_id='other-project' WHERE id='one'`)
				case "swarm-namespace":
					exec(`UPDATE swarms SET atespace_uid='other' WHERE id='one'`)
				case "interrupted":
					actor, err := s.GetManagedActor(ctx, "a")
					mustNil(t, err)
					sw, err := s.GetSwarm(ctx, "one")
					mustNil(t, err)
					_, err = s.AcquireOwnerActorOperation(ctx, actor, "parent-intervention", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
					mustNil(t, err)
				case "class":
					// Повреждённый snapshot отдельно от SQL-ограды: API обязан перепроверить class.
					exec(`DROP TRIGGER managed_actor_class_update_guard`)
					setActor(t, s, "isolated", "actor_class='persistent'")
				case "role":
					// CHECK уже запрещает такую роль; моделируем повреждение на одном соединении.
					mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
						_, err := s.conn(ctx).ExecContext(ctx, `PRAGMA ignore_check_constraints=ON; UPDATE managed_actors SET role='master' WHERE uid='isolated'; PRAGMA ignore_check_constraints=OFF`)
						return err
					}))
				case "parent":
					setActor(t, s, "isolated", "parent_uid='b'")
				case "swarm":
					setActor(t, s, "isolated", "swarm_id='two'")
				case "namespace":
					setActor(t, s, "isolated", "atespace_uid='other'")
				case "origin":
					setActor(t, s, "isolated", "origin='https://other.example/repo.git'")
				case "template":
					setActor(t, s, "isolated", "template='other'")
				case "fingerprint":
					setActor(t, s, "isolated", "source_fingerprint='other'")
				case "cluster":
					setActor(t, s, "isolated", "cluster_identity='other'")
				case "operation-id":
					setActor(t, s, "isolated", "operation_id='other'")
				case "operation":
					setActor(t, s, "isolated", "operation='start'")
				case "stage":
					setActor(t, s, "isolated", "stage='done'")
				case "phase":
					setActor(t, s, "isolated", "phase='error'")
				case "desired":
					setActor(t, s, "isolated", "desired='stopped'")
				case "upid":
					setActor(t, s, "isolated", "upid='task'")
				case "nonce":
					setActor(t, s, "isolated", "owner_nonce='other'")
				case "ctid":
					setActor(t, s, "isolated", "ctid=999")
				case "resources":
					setActor(t, s, "isolated", "cpu_millicores=999")
				case "source":
					exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git' WHERE swarm_id='one'`)
				case "pool", "pool-template", "pool-fingerprint", "pool-cluster":
					pool, err := s.GetManagedPool(ctx, "pool")
					mustNil(t, err)
					switch change {
					case "pool":
						pool.State = "draining"
					case "pool-template":
						pool.Template.URI = "other:template"
					case "pool-fingerprint":
						pool.Source.Network = "other"
					case "pool-cluster":
						pool.Source.ClusterIdentity = "other"
					}
					if change == "pool" {
						_, err = s.UpdateManagedPool(ctx, pool, pool.Revision)
						mustNil(t, err)
					} else {
						// API не меняет занятый pool binding; имитируем несовместимый snapshot.
						exec(`UPDATE managed_pools SET template=?,source=?,source_fingerprint=? WHERE uid='pool'`, encodeManaged(pool.Template), encodeManaged(pool.Source), sourceFingerprint(pool.Source))
					}
				case "stopped":
					sw, err := s.GetSwarm(ctx, "one")
					mustNil(t, err)
					_, err = s.StopSwarm(ctx, "one", sw.OwnerID, allow)
					mustNil(t, err)
				case "join":
					exec(`UPDATE swarm_child_joins SET state='aborted',revision=2,aborted_by='u',aborted_at='now'`)
				case "owner", "owner-settled":
					setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
					actor, err := s.GetManagedActor(ctx, "isolated")
					mustNil(t, err)
					sw, err := s.GetSwarm(ctx, "one")
					mustNil(t, err)
					op, err := s.AcquireOwnerActorOperation(ctx, actor, "intervene", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
					mustNil(t, err)
					if change == "owner-settled" {
						mustNil(t, s.FinishActorOperation(ctx, op, true))
					}
					setActor(t, s, "isolated", "phase='provisioning',stage='create',operation='create'")
				case "effect", "unknown-effect":
					op := ActorOperation{ProjectID: intent.Parent.Principal.ProjectID, SwarmID: intent.Parent.Principal.SwarmID, UID: "isolated", Generation: 1, OperationID: "effect", Caller: "trusted", Operation: "workspace_attest", State: "inflight"}
					exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), op.UID)
					if change == "unknown-effect" {
						mustNil(t, s.FinishActorOperation(ctx, op, false))
					}
				case "nil":
					auth = nil
				case "denied":
					auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
				}
				before := databaseRows(t, s.db)
				v, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, auth)
				if err == nil || v != (EphemeralChildCreateGrant{}) {
					t.Fatalf("stale scope granted create: %+v %v", v, err)
				}
				if err := s.CheckEphemeralChildCreate(ctx, intent.ID, auth); err == nil {
					t.Fatal("pre-call check accepted stale scope")
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("refused scope changed history")
				}
			})
		}
	}
}

func TestEphemeralCreateMissingHistoryNeverRepairedFromAllocation(t *testing.T) {
	ctx := context.Background()
	s, intent := createGrantFixture(t)
	// Моделируем отсутствие журнала, обойдя delete fence: allocation сам не даёт права.
	_, err := s.db.Exec(`DROP TRIGGER ephemeral_child_create_grant_delete_guard; DELETE FROM ephemeral_child_create_grants`)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	_, err = s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("isolated", 303), childAllow, nil)
	mustNil(t, err)
	if _, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing history became permission: %v", err)
	}
	source, err := s.GetEphemeralChildIntentForActor(ctx, "isolated", 1)
	mustNil(t, err)
	if source != intent || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("history lookup or allocation retry repaired absent grant")
	}
}

func TestEphemeralCreateReservedReleaseAndNestedRetriesNeverRearm(t *testing.T) {
	ctx := context.Background()
	s, intent := createGrantFixture(t)
	_, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
	mustNil(t, err)
	op := ActorOperation{ProjectID: intent.Parent.Principal.ProjectID, SwarmID: intent.Parent.Principal.SwarmID, UID: "isolated", Generation: 1, OperationID: "unrelated-workspace", Caller: "trusted", Operation: "workspace_attest", State: "inflight"}
	_, err = s.db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), op.UID)
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, false))
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	before := databaseRows(t, s.db)
	for _, run := range []func(context.Context, func(context.Context) error) error{s.WithTx, s.WithDurableTx} {
		mustNil(t, run(ctx, func(ctx context.Context) error {
			v, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
			if !errors.Is(err, ErrDurableTransactionNested) || v != (EphemeralChildCreateGrant{}) {
				t.Fatal("nested history retry returned create permission")
			}
			return nil
		}))
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if v, err := s.ReserveEphemeralChildCreate(canceled, intent.ID, childAllow); err == nil || v != (EphemeralChildCreateGrant{}) {
		t.Fatal("cancelled history retry returned permission")
	}
	retry, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
	mustNil(t, err)
	if !retry.Replay || retry.State != "reserved" || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("workspace release/cancellation/nesting rearmed create")
	}
}

func TestEphemeralCreateHistorySQLFences(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "reserved"}[reserved], func(t *testing.T) {
			s, intent := createGrantFixture(t)
			if reserved {
				_, err := s.ReserveEphemeralChildCreate(context.Background(), intent.ID, childAllow)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			for _, query := range []string{
				`DELETE FROM ephemeral_child_create_grants WHERE intent_id=?`,
				`INSERT OR REPLACE INTO ephemeral_child_create_grants SELECT * FROM ephemeral_child_create_grants WHERE intent_id=?`,
				`UPDATE ephemeral_child_create_grants SET state='queued',reserved_at='' WHERE intent_id=?`,
				`UPDATE ephemeral_child_create_grants SET state='legacy' WHERE intent_id=?`,
				`UPDATE ephemeral_child_create_grants SET created_at='other' WHERE intent_id=?`,
				`UPDATE ephemeral_child_create_grants SET reserved_at='other' WHERE intent_id=?`,
				`UPDATE ephemeral_child_create_grants SET intent_id='other' WHERE intent_id=?`,
			} {
				if _, err := s.db.Exec(query, intent.ID); err == nil {
					t.Fatalf("history rewrite succeeded: %s", query)
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("SQL refusal changed history")
				}
			}
		})
	}
}

func TestEphemeralCreateAllocationInitializesQueued(t *testing.T) {
	s, intent := allocationFixture(t, 1)
	addEphemeralCapacity(t, s)
	allocation, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	var state, created, reserved string
	err = s.db.QueryRow(`SELECT state,created_at,reserved_at FROM ephemeral_child_create_grants WHERE intent_id=?`, allocation.IntentID).Scan(&state, &created, &reserved)
	if err != nil {
		t.Fatalf("fresh allocation lacks atomic queued create grant: %v", err)
	}
	if state != "queued" || created != allocation.CreatedAt || reserved != "" {
		t.Fatal("allocation fabricated issued history instead of queued grant")
	}
}
