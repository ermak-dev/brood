package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func preparationFixture(t *testing.T) (*Store, PipelinePreparationRequest) {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	if _, err := s.db.Exec(`INSERT INTO project_members(project_id,user_id) VALUES ('one','u')`); err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(actorStartGraph, "template: development", "managedPool: pve", 1)
	source := PipelineSource{Origin: "https://git.example.org/team/repo", Commit: strings.Repeat("a", 40)}
	if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "unsupported", SwarmID: "one", OwnerID: "u", Graph: []byte(graph), Status: "running", Source: &source}); err == nil {
		t.Fatal("managed run became executable before dispatcher support")
	}
	if err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_runs(id,swarm_id,owner_id,graph,status) VALUES ('run','one','u',?,'running')`, []byte(graph)); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_sources(run_id,origin,commit_sha) VALUES ('run',?,?)`, source.Origin, source.Commit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s, PipelinePreparationRequest{SwarmID: "one", RunID: "run", NodeID: "actor", Sequence: 1, RequestID: "request", UserID: "u", Actor: managedRequest("dedicated", "one", 101)}
}

func TestPipelinePreparationReservesDedicatedActorOnce(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	allow := func(context.Context) bool { return true }
	const n = 8
	var wg sync.WaitGroup
	results := make(chan PipelinePreparation, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ReservePipelinePreparation(ctx, req, allow)
			results <- got
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	fresh := 0
	for got := range results {
		if got.ActorUID != req.Actor.UID || got.Incarnation != 1 {
			t.Fatalf("wrong actor: %+v", got)
		}
		if got.Replay {
			if got.Challenge != "" {
				t.Fatal("replay exposed challenge")
			}
		} else {
			fresh++
			if len(got.Challenge) != 64 {
				t.Fatalf("invalid challenge length %d", len(got.Challenge))
			}
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh reservations: %d", fresh)
	}
	actor, err := s.GetManagedActor(ctx, req.Actor.UID)
	if err != nil || actor.SwarmID != "one" || actor.Origin != "https://git.example.org/team/repo" {
		t.Fatalf("actor: %+v %v", actor, err)
	}
	visits, err := s.ListPipelineVisits(ctx, "one", "run")
	if err != nil || len(visits) != 1 || visits[0].State != "preparing" || visits[0].NodeID != req.NodeID || visits[0].Sequence != req.Sequence || visits[0].ActorUID != req.Actor.UID || visits[0].ActorIncarnation != 1 || visits[0].RequestID != req.RequestID {
		t.Fatalf("preparation not bound to visit: %+v %v", visits, err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "one", "run", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: req.Actor.UID, ActorIncarnation: 1, RequestID: req.RequestID}); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("preparation authorized dispatch: %v", err)
	}
}

func TestPipelinePreparationRefusesExistingActorAndChangedRequest(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	allow := func(context.Context) bool { return true }
	if _, err := s.ReserveManagedActor(ctx, req.Actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReservePipelinePreparation(ctx, req, allow); err == nil {
		t.Fatal("reused an existing actor")
	}
	if _, err := s.GetPipelineRun(ctx, "one", "run"); err != nil {
		t.Fatal(err)
	}
}

func TestPipelinePreparationRejectsChangedReplay(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	allow := func(context.Context) bool { return true }
	if _, err := s.ReservePipelinePreparation(ctx, req, allow); err != nil {
		t.Fatal(err)
	}
	changed := req
	changed.Actor.CTID++
	if _, err := s.ReservePipelinePreparation(ctx, changed, allow); !errors.Is(err, ErrPipelinePreparationConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	if _, err := s.ReservePipelinePreparation(ctx, req, func(context.Context) bool { return false }); err == nil {
		t.Fatal("replay after authorization revoked was accepted")
	}
}

func TestPipelinePreparationPreventsActorMutationBeforeVerification(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	allow := func(context.Context) bool { return true }
	if _, err := s.ReservePipelinePreparation(ctx, req, allow); err != nil {
		t.Fatal(err)
	}
	actor, err := s.GetManagedActor(ctx, req.Actor.UID)
	if err != nil {
		t.Fatal(err)
	}
	actor.Phase = "error"
	if _, err := s.UpdateManagedActor(ctx, actor, actor.Revision); err == nil {
		t.Fatal("unverified preparation actor was modified")
	}
	stored, err := s.GetManagedActor(ctx, req.Actor.UID)
	if err != nil || stored.Phase != "provisioning" {
		t.Fatalf("actor changed: %+v %v", stored, err)
	}
	if replay, err := s.ReservePipelinePreparation(ctx, req, allow); err != nil || !replay.Replay {
		t.Fatalf("reservation replay: %+v %v", replay, err)
	}
}

func TestPipelinePreparationBindingIsImmutable(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	if _, err := s.ReservePipelinePreparation(ctx, req, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	for _, recursive := range []string{"OFF", "ON"} {
		if _, err := s.db.Exec(`PRAGMA recursive_triggers=` + recursive); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`DELETE FROM pipeline_preparations WHERE run_id='run'`,
			`UPDATE pipeline_preparations SET request_digest='forged' WHERE run_id='run'`,
			`INSERT OR REPLACE INTO pipeline_preparations SELECT run_id,sequence,node_id,request_id,actor_uid,actor_incarnation,pool_uid,pool_revision,request_digest,printf('%064d',0),state,created_at FROM pipeline_preparations WHERE run_id='run'`,
			`DELETE FROM pipeline_steps WHERE run_id='run'`,
			`UPDATE pipeline_steps SET actor_uid='other',node_id='other',state='completed' WHERE run_id='run' AND sequence=1`,
			`INSERT INTO pipeline_steps(run_id,sequence,node_id,state) VALUES ('run',2,'actor','pending')`,
			`INSERT OR REPLACE INTO pipeline_steps(run_id,sequence,node_id,state) VALUES ('run',1,'actor','pending')`,
			`INSERT OR REPLACE INTO managed_actors SELECT * FROM managed_actors WHERE uid='dedicated'`,
		} {
			if _, err := s.db.Exec(statement); err == nil {
				t.Fatalf("binding was mutable through %s", statement)
			}
		}
	}
	if _, err := s.db.Exec(`DELETE FROM pipeline_runs WHERE id='run'`); err == nil {
		t.Fatal("deleted run while its actor reservation is unresolved")
	}
	if _, err := s.GetPipelineRun(ctx, "one", "run"); err != nil {
		t.Fatalf("run lost after refused deletion: %v", err)
	}
	if _, err := s.GetManagedActor(ctx, req.Actor.UID); err != nil {
		t.Fatalf("reserved actor lost: %v", err)
	}
}

func TestPipelinePreparationBlocksParentDeletion(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	if _, err := s.ReservePipelinePreparation(ctx, req, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM pipeline_runs WHERE id='run'`); err == nil {
		t.Fatal("deleted run while its actor reservation is unresolved")
	}
	if _, err := s.GetPipelineRun(ctx, "one", "run"); err != nil {
		t.Fatal(err)
	}
}

func TestPipelinePreparationRejectsStaleVisitAndPoolRevision(t *testing.T) {
	for _, scenario := range []string{"visit", "pool"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, req := preparationFixture(t)
			allow := func(context.Context) bool { return true }
			if _, err := s.ReservePipelinePreparation(ctx, req, allow); err != nil {
				t.Fatal(err)
			}
			if scenario == "visit" {
				if _, err := s.db.Exec(`DROP TRIGGER pipeline_preparations_step_update_fence; UPDATE pipeline_steps SET state='completed' WHERE run_id='run' AND sequence=1`); err != nil {
					t.Fatal(err)
				}
			} else {
				pool, err := s.GetManagedPool(ctx, "pool")
				if err != nil {
					t.Fatal(err)
				}
				pool.Labels["revision"] = "changed"
				if _, err := s.UpdateManagedPool(ctx, pool, pool.Revision); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReservePipelinePreparation(ctx, req, allow); !errors.Is(err, ErrPipelinePreparationConflict) {
				t.Fatalf("stale %s replay: %v", scenario, err)
			}
		})
	}
}

func TestPipelinePreparationRecoveryMarksUnconfirmed(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	allow := func(context.Context) bool { return true }
	if _, err := s.ReservePipelinePreparation(ctx, req, allow); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverPipelinePreparations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverPipelinePreparations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReservePipelinePreparation(ctx, req, allow); !errors.Is(err, ErrPipelinePreparationConflict) {
		t.Fatalf("replay after recovery: %v", err)
	}
	visits, err := s.ListPipelineVisits(ctx, "one", "run")
	if err != nil || len(visits) != 1 || visits[0].State != "unconfirmed" {
		t.Fatalf("recovered visit: %+v %v", visits, err)
	}
	if _, err := s.GetManagedActor(ctx, req.Actor.UID); err != nil {
		t.Fatalf("reservation lost actor: %v", err)
	}
	for _, statement := range []string{
		`UPDATE pipeline_preparations SET state='reserved' WHERE run_id='run'`,
		`UPDATE pipeline_steps SET state='preparing' WHERE run_id='run'`,
		`DELETE FROM pipeline_runs WHERE id='run'`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("unconfirmed preparation could be revived or removed through %s", statement)
		}
	}
}

func TestPipelinePreparationRechecksAuthorizationAndRollsBack(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	if _, err := s.ReservePipelinePreparation(ctx, req, func(context.Context) bool { return false }); err == nil {
		t.Fatal("unauthorized reservation accepted")
	}
	if _, err := s.GetManagedActor(ctx, req.Actor.UID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("actor was reserved: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM project_members WHERE project_id='one' AND user_id='u'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReservePipelinePreparation(ctx, req, func(context.Context) bool { return true }); err == nil {
		t.Fatal("revoked member accepted")
	}
	if _, err := s.GetManagedActor(ctx, req.Actor.UID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked actor reserved: %v", err)
	}
}

func TestPipelinePreparationInsertFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_preparation BEFORE INSERT ON pipeline_preparations BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReservePipelinePreparation(ctx, req, func(context.Context) bool { return true }); err == nil {
		t.Fatal("preparation unexpectedly succeeded")
	}
	if _, err := s.GetManagedActor(ctx, req.Actor.UID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial actor persisted: %v", err)
	}
}

func TestPipelinePreparationRefusesOuterTransaction(t *testing.T) {
	ctx := context.Background()
	s, req := preparationFixture(t)
	if err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.ReservePipelinePreparation(ctx, req, func(context.Context) bool { return true }); err == nil {
			return errors.New("preparation unexpectedly succeeded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetManagedActor(ctx, req.Actor.UID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial actor persisted: %v", err)
	}
}
