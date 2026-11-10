package store

// FEATURE: Подготовка выделенного актора под узел закрыта (#208).
// SPEC: Узел называет роль, рой связывает её с существующим актором и не
// создаёт актора на узел (#204). ReservePipelinePreparation резервировала
// нового managed-актора под посещение узла; теперь она отказывает
// ErrPipelineNodeActorCreation до любого чтения и записи. Таблица
// pipeline_preparations и её триггеры остаются в схеме, их гарантии
// проверяются на строке, записанной так, как это делала подготовка.
// IMPLEMENTATION: Функция-заглушка с явной ошибкой; триггеры (#163, #206)
// не тронуты.
// RESULT: IMPLEMENTED — подготовка не создаёт актора, шага и намерения ни
// при какой привязке роли; уже записанное намерение неизменно.
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// preparationFixture — рой «one» с графом actor-start (роль build),
// снимком источника и участником u: всё, что прежде допускало подготовку.
func preparationFixture(t *testing.T) (*Store, PipelinePreparationRequest) {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	if _, err := s.db.Exec(`INSERT INTO project_members(project_id,user_id) VALUES ('one','u')`); err != nil {
		t.Fatal(err)
	}
	source := PipelineSource{Origin: "https://git.example.org/team/repo", Commit: strings.Repeat("a", 40)}
	if err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET graph=?,execution='running',definition_name='release',definition_version=1,definition_revision='rev',updated_at='now' WHERE id='one'`, []byte(actorStartGraph)); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_sources(swarm_id,origin,commit_sha) VALUES ('one',?,?)`, source.Origin, source.Commit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s, PipelinePreparationRequest{SwarmID: "one", RunID: "one", NodeID: "actor", Sequence: 1, RequestID: "request", UserID: "u", Actor: managedRequest("dedicated", "one", 101)}
}

// TestPipelinePreparationClosed — подготовка отказывает при любом запросе:
// новый актор под узел, привязанный к роли актор, отозванное разрешение,
// внешняя транзакция. Ни актора, ни шага, ни намерения не остаётся, а
// привязанный актор не меняется.
func TestPipelinePreparationClosed(t *testing.T) {
	ctx := context.Background()
	allow := func(context.Context) bool { return true }
	deny := func(context.Context) bool { return false }
	for _, tc := range []struct {
		name      string
		actor     func(bound ManagedActor) ManagedActor
		authorize func(context.Context) bool
		nested    bool
	}{
		{"new dedicated actor", func(ManagedActor) ManagedActor { return managedRequest("dedicated", "one", 101) }, allow, false},
		{"the bound actor itself", func(b ManagedActor) ManagedActor { return managedRequest(b.UID, "one", b.CTID) }, allow, false},
		{"unauthorized", func(ManagedActor) ManagedActor { return managedRequest("dedicated", "one", 101) }, deny, false},
		{"inside a transaction", func(ManagedActor) ManagedActor { return managedRequest("dedicated", "one", 101) }, allow, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, req := preparationFixture(t)
			bound := roleActor(t, s, "one", "bound", "build")
			req.Actor = tc.actor(bound)
			var err error
			if tc.nested {
				_ = s.WithTx(ctx, func(ctx context.Context) error {
					_, err = s.ReservePipelinePreparation(ctx, req, tc.authorize)
					return err
				})
			} else {
				_, err = s.ReservePipelinePreparation(ctx, req, tc.authorize)
			}
			if !errors.Is(err, ErrPipelineNodeActorCreation) {
				t.Fatalf("preparation: %v, want %v", err, ErrPipelineNodeActorCreation)
			}
			var actors, steps, preparations int
			if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM managed_actors),(SELECT count(*) FROM swarm_steps),(SELECT count(*) FROM pipeline_preparations)`).Scan(&actors, &steps, &preparations); err != nil {
				t.Fatal(err)
			}
			if actors != 1 || steps != 0 || preparations != 0 {
				t.Fatalf("closed preparation left actors=%d steps=%d preparations=%d", actors, steps, preparations)
			}
			if got, err := s.GetManagedActor(ctx, bound.UID); err != nil || got.Revision != bound.Revision || got.Phase != bound.Phase {
				t.Fatalf("bound actor changed: %+v %v", got, err)
			}
		})
	}
}

// seedPreparation кладёт намерение так, как его записывала подготовка:
// актор «dedicated» в резерве, шаг preparing и строка pipeline_preparations.
func seedPreparation(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ReserveManagedActor(ctx, managedRequest("dedicated", "one", 101)); err != nil {
		t.Fatal(err)
	}
	pool, err := s.GetManagedPool(ctx, "pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,started_at,actor_uid,actor_incarnation,request_id) VALUES ('one',1,'actor','preparing','now','dedicated',1,'request')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO pipeline_preparations(swarm_id,sequence,node_id,request_id,actor_uid,actor_incarnation,pool_uid,pool_revision,request_digest,challenge_digest,state,created_at) VALUES ('one',1,'actor','request','dedicated',1,'pool',?,?,?,'reserved','now')`, pool.Revision, strings.Repeat("d", 64), strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestPipelinePreparationPreventsActorMutationBeforeVerification(t *testing.T) {
	ctx := context.Background()
	s, _ := preparationFixture(t)
	seedPreparation(t, s)
	actor, err := s.GetManagedActor(ctx, "dedicated")
	if err != nil {
		t.Fatal(err)
	}
	actor.Phase = "error"
	if _, err := s.UpdateManagedActor(ctx, actor, actor.Revision); err == nil {
		t.Fatal("unverified preparation actor was modified")
	}
	stored, err := s.GetManagedActor(ctx, "dedicated")
	if err != nil || stored.Phase != "provisioning" {
		t.Fatalf("actor changed: %+v %v", stored, err)
	}
}

func TestPipelinePreparationBindingIsImmutable(t *testing.T) {
	ctx := context.Background()
	s, _ := preparationFixture(t)
	seedPreparation(t, s)
	for _, recursive := range []string{"OFF", "ON"} {
		if _, err := s.db.Exec(`PRAGMA recursive_triggers=` + recursive); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`DELETE FROM pipeline_preparations WHERE swarm_id='one'`,
			`UPDATE pipeline_preparations SET request_digest='forged' WHERE swarm_id='one'`,
			`INSERT OR REPLACE INTO pipeline_preparations SELECT swarm_id,sequence,node_id,request_id,actor_uid,actor_incarnation,pool_uid,pool_revision,request_digest,printf('%064d',0),state,created_at FROM pipeline_preparations WHERE swarm_id='one'`,
			`DELETE FROM swarm_steps WHERE swarm_id='one' AND sequence=1`,
			`UPDATE swarm_steps SET actor_uid='other',node_id='other',state='completed' WHERE swarm_id='one' AND sequence=1`,
			`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('one',2,'actor','pending')`,
			`INSERT OR REPLACE INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('one',1,'actor','pending')`,
			`INSERT OR REPLACE INTO managed_actors SELECT * FROM managed_actors WHERE uid='dedicated'`,
		} {
			if _, err := s.db.Exec(statement); err == nil {
				t.Fatalf("binding was mutable through %s", statement)
			}
		}
	}
	if _, err := s.db.Exec(`DELETE FROM swarms WHERE id='one'`); err == nil {
		t.Fatal("deleted swarm while its actor reservation is unresolved")
	}
	if _, err := s.GetSwarm(ctx, "one"); err != nil {
		t.Fatalf("swarm lost after refused deletion: %v", err)
	}
	if _, err := s.GetManagedActor(ctx, "dedicated"); err != nil {
		t.Fatalf("reserved actor lost: %v", err)
	}
}

func TestPipelinePreparationBlocksParentDeletion(t *testing.T) {
	ctx := context.Background()
	s, _ := preparationFixture(t)
	seedPreparation(t, s)
	if _, err := s.db.Exec(`DELETE FROM swarms WHERE id='one'`); err == nil {
		t.Fatal("deleted swarm while its actor reservation is unresolved")
	}
	if _, err := s.GetSwarm(ctx, "one"); err != nil {
		t.Fatal(err)
	}
}

func TestPipelinePreparationRecoveryMarksUnconfirmed(t *testing.T) {
	ctx := context.Background()
	s, _ := preparationFixture(t)
	seedPreparation(t, s)
	if err := s.RecoverPipelinePreparations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverPipelinePreparations(ctx); err != nil {
		t.Fatal(err)
	}
	visits, err := s.SwarmSteps(ctx, "one")
	if err != nil || len(visits) != 1 || visits[0].State != "unconfirmed" {
		t.Fatalf("recovered visit: %+v %v", visits, err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM pipeline_preparations WHERE swarm_id='one'`).Scan(&state); err != nil || state != "unconfirmed" {
		t.Fatalf("recovered preparation: %q %v", state, err)
	}
	if _, err := s.GetManagedActor(ctx, "dedicated"); err != nil {
		t.Fatalf("reservation lost actor: %v", err)
	}
	for _, statement := range []string{
		`UPDATE pipeline_preparations SET state='reserved' WHERE swarm_id='one'`,
		`UPDATE swarm_steps SET state='preparing' WHERE swarm_id='one'`,
		`DELETE FROM swarms WHERE id='one'`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("unconfirmed preparation could be revived or removed through %s", statement)
		}
	}
}
