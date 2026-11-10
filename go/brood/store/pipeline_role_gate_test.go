package store

// FEATURE: Шаг actor-узла адресует актора, привязанного к роли узла (#208).
// SPEC: Узел называет роль, рой связывает её с существующим актором и не
// создаёт актора на узел (#204). Путь, который пишет actor_uid и инкарнацию
// в шаг, в той же транзакции разрешает роль узла и требует совпадения UID
// и текущей инкарнации; иначе отказ без записи.
// IMPLEMENTATION: appendPipelineVisit зовёт requireRoleActor внутри своей
// транзакции до записи шага, для первого шага и для ожидающего.
// RESULT: IMPLEMENTED — шаг нельзя записать для актора, не привязанного к
// роли узла, для другой или устаревшей инкарнации, для актора чужого роя
// или неживого актора.
// STATUS: DONE

import (
	"context"
	"errors"
	"testing"
)

// roleActor заводит живого managed-актора uid в рое swarmID и, если role не
// пуста, привязывает к нему роль. Рой переводится в active со своим
// atespace; пул «pool» создаётся при первом вызове с запасом: вместимость
// пула эти тесты не проверяют (#221).
func roleActor(t *testing.T, s *Store, swarmID, uid, role string) ManagedActor {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE swarms SET state='active', atespace_uid=COALESCE(atespace_uid,'native-'||id) WHERE id=?`, swarmID); err != nil {
		t.Fatal(err)
	}
	sw, err := s.GetSwarm(ctx, swarmID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetManagedPool(ctx, "pool"); errors.Is(err, ErrNotFound) {
		if _, err := s.CreateManagedPool(ctx, ManagedPool{UID: "pool", Name: "pve", Connection: "configured", Source: ManagedSourceSnapshot{ClusterIdentity: "cluster", Endpoint: "https://pve.invalid:8006", Node: "node", ResourcePool: "brood", Storage: "local", Network: "vmbr0"}, Labels: map[string]string{"provider": "proxmox"}, MaxActors: 16, Resources: ManagedResources{CPUMillicores: 16000, MemoryBytes: 16384, DiskBytes: 32768}, Template: ManagedTemplateSnapshot{Profile: "claude-proxmox", URI: "local:template.tar.zst", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, State: "enabled"}); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	var ctid int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(ctid),500)+1 FROM managed_actors`).Scan(&ctid); err != nil {
		t.Fatal(err)
	}
	request := managedRequest(uid, swarmID, ctid)
	request.AtespaceUID = *sw.AtespaceUID
	actor, err := s.ReserveManagedActor(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: swarmID, Role: role, ActorUID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	return actor
}

// rebindAt переводит актора на инкарнацию incarnation и заново привязывает
// к нему роль: так выглядит перезапуск актора с последующей перепривязкой.
func rebindAt(t *testing.T, s *Store, swarmID, uid, role string, incarnation int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE managed_actors SET incarnation=? WHERE uid=?`, incarnation, uid); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSwarmRole(context.Background(), SwarmRoleBinding{SwarmID: swarmID, Role: role, ActorUID: uid}); err != nil {
		t.Fatal(err)
	}
}

// assertNoActorStep — отказ не оставил ни шага с актором, ни нового шага
// сверх ожидаемых.
func assertNoActorStep(t *testing.T, s *Store, swarmID string, steps int) {
	t.Helper()
	var total, addressed int
	if err := s.db.QueryRow(`SELECT count(*),count(actor_uid) FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&total, &addressed); err != nil {
		t.Fatal(err)
	}
	if total != steps || addressed != 0 {
		t.Fatalf("refused step left %d steps (want %d), %d with an actor", total, steps, addressed)
	}
}

// roleGateCases — запросы шага для узла с ролью build, привязанной к actor-a
// роя «s»; ни один не адресует привязанного актора нынешней инкарнации.
// drift выполняется между привязкой и запросом.
var roleGateCases = []struct {
	name, drift, uid string
	incarnation      int64
	want             error
}{
	{"another live actor of the swarm", "", "actor-b", 1, ErrSwarmRoleMismatch},
	{"bound actor at another incarnation", "", "actor-a", 2, ErrSwarmRoleMismatch},
	{"actor of a foreign swarm", "", "actor-c", 1, ErrSwarmRoleMismatch},
	{"unknown actor", "", "missing", 1, ErrSwarmRoleMismatch},
	{"incarnation advanced, old incarnation", `UPDATE managed_actors SET incarnation=2 WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleStale},
	{"incarnation advanced, new incarnation", `UPDATE managed_actors SET incarnation=2 WHERE uid='actor-a'`, "actor-a", 2, ErrSwarmRoleStale},
	{"bound actor moved to a foreign swarm", `UPDATE managed_actors SET swarm_id='two' WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleForeignActor},
	{"bound actor in a stale atespace", `UPDATE managed_actors SET atespace_uid='stale' WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleForeignActor},
	{"bound actor deleting", `UPDATE managed_actors SET phase='deleting' WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleInactive},
	{"bound actor deleted", `UPDATE managed_actors SET phase='deleted' WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleInactive},
	{"bound actor desired deleted", `UPDATE managed_actors SET desired='deleted' WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleInactive},
	{"bound actor stopped", `UPDATE managed_actors SET phase='stopped' WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleInactive},
	{"bound actor failed", `UPDATE managed_actors SET phase='error' WHERE uid='actor-a'`, "actor-a", 1, ErrSwarmRoleInactive},
	{"swarm deleting", `UPDATE swarms SET state='deleting' WHERE id='s'`, "actor-a", 1, ErrSwarmRoleInactive},
	{"role unbound", `DELETE FROM swarm_role_bindings WHERE swarm_id='s'`, "actor-a", 1, ErrSwarmRoleUnbound},
	{"role bound only in the actor's own foreign swarm", `DELETE FROM swarm_role_bindings WHERE swarm_id='s'`, "actor-c", 1, ErrSwarmRoleUnbound},
}

// roleGateStore — рой «s» с графом graph; роль build привязана к actor-a,
// actor-b — живой актор того же роя с ролью review, actor-c — живой актор
// роя «two», где роль build привязана к нему.
func roleGateStore(t *testing.T, graph string) *Store {
	t.Helper()
	s := sourceTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('two','p','u','two','space-two','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err := attachTestGraph(context.Background(), s, "s", graph); err != nil {
		t.Fatal(err)
	}
	roleActor(t, s, "s", "actor-a", "build")
	roleActor(t, s, "s", "actor-b", "review")
	roleActor(t, s, "two", "actor-c", "build")
	return s
}

// TestPipelineFirstStepAddressesBoundActor — первый шаг графа пишется только
// для актора, привязанного к роли стартового узла, в нынешней инкарнации.
func TestPipelineFirstStepAddressesBoundActor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range roleGateCases {
		t.Run(tc.name, func(t *testing.T) {
			s := roleGateStore(t, actorStartGraph)
			if tc.drift != "" {
				if _, err := s.db.Exec(tc.drift); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: tc.uid, ActorIncarnation: tc.incarnation, RequestID: "req"})
			if !errors.Is(err, ErrPipelineVisitConflict) || !errors.Is(err, tc.want) || errors.Is(err, ErrNotFound) {
				t.Fatalf("step for %s@%d: %v, want a conflict caused by %v", tc.uid, tc.incarnation, err, tc.want)
			}
			assertNoActorStep(t, s, "s", 0)
		})
	}
	t.Run("bound actor at its current incarnation", func(t *testing.T) {
		s := roleGateStore(t, actorStartGraph)
		v, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor-a", ActorIncarnation: 1, RequestID: "req"})
		if err != nil || v.Replay || v.Sequence != 1 {
			t.Fatalf("bound actor step: %+v %v", v, err)
		}
		steps, err := s.SwarmSteps(ctx, "s")
		if err != nil || len(steps) != 1 || steps[0].State != "inflight" || steps[0].ActorUID != "actor-a" || steps[0].ActorIncarnation != 1 {
			t.Fatalf("stored step: %+v %v", steps, err)
		}
	})
	t.Run("rebound after a new incarnation", func(t *testing.T) {
		s := roleGateStore(t, actorStartGraph)
		rebindAt(t, s, "s", "actor-a", "build", 2)
		if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor-a", ActorIncarnation: 1, RequestID: "req"}); !errors.Is(err, ErrSwarmRoleMismatch) {
			t.Fatalf("previous incarnation after rebind: %v", err)
		}
		if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor-a", ActorIncarnation: 2, RequestID: "req"}); err != nil {
			t.Fatalf("rebound incarnation: %v", err)
		}
	})
	t.Run("one actor serves several roles", func(t *testing.T) {
		s := roleGateStore(t, actorStartGraph)
		if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "s", Role: "build", ActorUID: "actor-b"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor-a", ActorIncarnation: 1, RequestID: "req"}); !errors.Is(err, ErrSwarmRoleMismatch) {
			t.Fatalf("role moved to actor-b still admits actor-a: %v", err)
		}
		if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor-b", ActorIncarnation: 1, RequestID: "req"}); err != nil {
			t.Fatalf("actor-b holding review and build: %v", err)
		}
	})
}

// TestPipelinePendingStepAddressesBoundActor — то же для ожидающего шага,
// который создал переход графа: отказ оставляет шаг pending без актора.
func TestPipelinePendingStepAddressesBoundActor(t *testing.T) {
	ctx := context.Background()
	pending := func(t *testing.T) *Store {
		t.Helper()
		s := roleGateStore(t, runGraph)
		if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
			t.Fatal(err)
		}
		if err := s.AnswerPipelineVisit(ctx, "s", "s", 1, "success"); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, tc := range roleGateCases {
		t.Run(tc.name, func(t *testing.T) {
			s := pending(t)
			if tc.drift != "" {
				if _, err := s.db.Exec(tc.drift); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: tc.uid, ActorIncarnation: tc.incarnation, RequestID: "req"})
			if !errors.Is(err, ErrPipelineVisitConflict) || !errors.Is(err, tc.want) || errors.Is(err, ErrNotFound) {
				t.Fatalf("step for %s@%d: %v, want a conflict caused by %v", tc.uid, tc.incarnation, err, tc.want)
			}
			assertNoActorStep(t, s, "s", 2)
			var state string
			if err := s.db.QueryRow(`SELECT state FROM swarm_steps WHERE swarm_id='s' AND sequence=2`).Scan(&state); err != nil || state != "pending" {
				t.Fatalf("refused step changed state to %q (%v)", state, err)
			}
		})
	}
	t.Run("bound actor at its current incarnation", func(t *testing.T) {
		s := pending(t)
		v, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor-a", ActorIncarnation: 1, RequestID: "req"})
		if err != nil || v.Sequence != 2 {
			t.Fatalf("bound actor step: %+v %v", v, err)
		}
	})
}

// TestPipelineRoleCheckSharesStepTransaction — проверка роли читает состояние
// транзакции, в которой пишется шаг: выведение актора, ещё не
// зафиксированное этой же транзакцией, уже закрывает шаг. Отдельный вызов
// проверки вне транзакции записи этого бы не увидел.
func TestPipelineRoleCheckSharesStepTransaction(t *testing.T) {
	ctx := context.Background()
	s := roleGateStore(t, actorStartGraph)
	rollback := errors.New("rollback")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='deleting' WHERE uid='actor-a'`); err != nil {
			return err
		}
		if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor-a", ActorIncarnation: 1, RequestID: "req"}); !errors.Is(err, ErrSwarmRoleInactive) {
			t.Errorf("step written against an actor retired in the same transaction: %v", err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	assertNoActorStep(t, s, "s", 0)
}

// TestRequireRoleActorRefusesOutsideTransaction — проверка роли вне
// транзакции записи — только совет, поэтому она отказывает сама, даже для
// привязанного актора нынешней инкарнации; внутри транзакции допускает его.
func TestRequireRoleActorRefusesOutsideTransaction(t *testing.T) {
	ctx := context.Background()
	s := roleGateStore(t, actorStartGraph)
	if err := s.requireRoleActor(ctx, "s", "build", "actor-a", 1); err == nil {
		t.Fatal("role check outside the step transaction admitted the actor")
	}
	if err := s.WithTx(ctx, func(ctx context.Context) error {
		return s.requireRoleActor(ctx, "s", "build", "actor-a", 1)
	}); err != nil {
		t.Fatalf("role check inside a transaction: %v", err)
	}
}
