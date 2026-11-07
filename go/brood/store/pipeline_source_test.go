package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func createLegacyPipelineRun(ctx context.Context, s *Store, r PipelineRun) error {
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_runs(id,swarm_id,owner_id,graph,status) VALUES (?,?,?,?,?)`, r.ID, r.SwarmID, r.OwnerID, r.Graph, r.Status)
	return err
}

func sourceTestStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'); INSERT INTO swarm_members VALUES ('s','u','owner')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPipelineSourceSnapshot(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	source := &PipelineSource{Origin: "https://git.example.org/team/repo.git", Commit: strings.Repeat("a", 40)}
	run := PipelineRun{ID: "bound", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running", Source: source}
	if err := s.CreatePipelineRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	source.Origin = "https://other.example.org/repo"
	got, err := s.GetPipelineRun(ctx, "s", "bound")
	if err != nil || got.Source == nil || got.Source.Origin != "https://git.example.org/team/repo.git" || got.Source.Commit != strings.Repeat("a", 40) {
		t.Fatalf("snapshot: %+v %v", got.Source, err)
	}
	if _, err := s.GetPipelineRun(ctx, "other", "bound"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-swarm lookup: %v", err)
	}
	list, err := s.ListPipelineRuns(ctx, "s")
	if err != nil || len(list) != 1 || list[0].Source == nil || *list[0].Source != *got.Source {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, recursive := range []int{0, 1} {
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA recursive_triggers=%d`, recursive)); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`UPDATE pipeline_sources SET origin='https://changed.example.org/repo' WHERE run_id='bound'`,
			`DELETE FROM pipeline_sources WHERE run_id='bound'`,
			`INSERT OR REPLACE INTO pipeline_sources(run_id,origin,commit_sha) VALUES ('bound','https://changed.example.org/repo','0000000000000000000000000000000000000000')`,
			`INSERT OR REPLACE INTO pipeline_runs(id,swarm_id,owner_id,graph,status) VALUES ('bound','s','u',x'00','running')`,
		} {
			if _, err := s.db.Exec(statement); err == nil {
				t.Fatalf("source snapshot was mutable through %s", statement)
			}
		}
	}
	if _, err := s.db.Exec(`DELETE FROM pipeline_runs WHERE id='bound'`); err != nil {
		t.Fatalf("run cascade: %v", err)
	}
}

func TestPipelineRunRequiresSource(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	err := s.CreatePipelineRun(ctx, PipelineRun{ID: "new", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running"})
	if err == nil {
		t.Fatal("new run without a source was accepted")
	}
	if _, err := s.GetPipelineRun(ctx, "s", "new"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound run persisted: %v", err)
	}
}

func TestPipelineSourceInsertRollback(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_pipeline_source BEFORE INSERT ON pipeline_sources BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.CreatePipelineRun(ctx, PipelineRun{ID: "partial", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running", Source: &PipelineSource{Origin: "https://git.example.org/repo", Commit: strings.Repeat("a", 40)}})
	if err == nil {
		t.Fatal("source insert unexpectedly succeeded")
	}
	if _, err := s.GetPipelineRun(ctx, "s", "partial"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial run persisted: %v", err)
	}
}

func TestPipelineSourceNestedInsertRollback(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_pipeline_source BEFORE INSERT ON pipeline_sources BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		err := s.CreatePipelineRun(ctx, PipelineRun{ID: "partial", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running", Source: &PipelineSource{Origin: "https://git.example.org/repo", Commit: strings.Repeat("a", 40)}})
		if err == nil {
			return errors.New("source insert unexpectedly succeeded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPipelineRun(ctx, "s", "partial"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial run persisted after outer commit: %v", err)
	}
}

func TestPipelineSourceValidationAndNoPartialRun(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	for i, source := range []PipelineSource{
		{Origin: "https://git.example.org/repo", Commit: ""},
		{Origin: "", Commit: strings.Repeat("a", 40)},
		{Origin: "https://user:secret@git.example.org/repo", Commit: strings.Repeat("a", 40)},
		{Origin: "https://git.example.org/repo", Commit: strings.Repeat("A", 40)},
		{Origin: "https://git.example.org/repo", Commit: strings.Repeat("a", 39)},
	} {
		run := PipelineRun{ID: "bad", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running", Source: &source}
		if err := s.CreatePipelineRun(ctx, run); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("case %d: %v", i, err)
		}
		if _, err := s.GetPipelineRun(ctx, "s", "bad"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("case %d inserted run: %v", i, err)
		}
	}
}

func TestBoundPipelineRefusesActorEffects(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	run := PipelineRun{ID: "bound", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running", Source: &PipelineSource{Origin: "https://git.example.org/repo", Commit: strings.Repeat("a", 40)}}
	if err := s.CreatePipelineRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	visit := PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "uid", ActorIncarnation: 1, RequestID: "request"}
	if _, err := s.AppendPipelineVisit(ctx, "s", "bound", visit); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("inflight: %v", err)
	}
	reservation := PipelineNativeReservation{SwarmID: "s", RunID: "bound", Sequence: 1, NodeID: "actor", RequestID: "request", TemplateName: "development", TemplateUID: "template-uid", TemplateDigest: "sha256:" + strings.Repeat("a", 64)}
	if _, err := s.ReservePipelineNativeActor(ctx, reservation); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("reservation: %v", err)
	}
	if err := s.BindPipelineNativeActor(ctx, reservation, "uid", 1); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("binding: %v", err)
	}
	if err := s.CompletePipelineActorVisit(ctx, "s", "bound", 1, "request", "uid", 1, "success"); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("completion: %v", err)
	}
	visits, err := s.ListPipelineVisits(ctx, "s", "bound")
	if err != nil || len(visits) != 0 {
		t.Fatalf("unexpected visit: %+v %v", visits, err)
	}
}

func TestPipelineSourceLegacyMigration(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO pipeline_runs(id,swarm_id,owner_id,graph,status) VALUES ('legacy','s','u',?,'running')`, []byte(actorStartGraph)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER pipeline_runs_no_replace; DROP TABLE pipeline_sources; PRAGMA user_version=14`); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPipelineRun(ctx, "s", "legacy")
	if err != nil || got.Source != nil {
		t.Fatalf("legacy snapshot: %+v %v", got.Source, err)
	}
	if err := s.initializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	visit := PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "uid", ActorIncarnation: 1, RequestID: "legacy-request"}
	if _, err := s.AppendPipelineVisit(ctx, "s", "legacy", visit); err != nil {
		t.Fatalf("migrated legacy run no longer accepts its actor visit: %v", err)
	}
}
