package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func sourceTestStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'); INSERT INTO swarm_members VALUES ('s','u','owner')`); err != nil {
		t.Fatal(err)
	}
	return s
}

// FEATURE: Привязка снимка графа к рою с origin источника (#206).
// SPEC: AttachSwarmGraph валидирует граф и личность определения, origin
// обязателен, привязка атомарна и одинока: второй граф не принимается.
// Commit закрепляет проход, а не привязка (#229).
// RESULT: IMPLEMENTED — снимок и origin живут на рое.
// STATUS: DONE
func TestAttachSwarmGraph(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	const source = "https://git.example.org/team/repo.git"
	if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 3, "cafebabedeadbeefcafebabedeadbeef", source); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSwarm(ctx, "s")
	if err != nil || string(got.Graph) != actorStartGraph || got.Execution != "idle" {
		t.Fatalf("attached swarm: graph=%v execution=%q err=%v", got.Graph, got.Execution, err)
	}
	if got.DefinitionName != "release" || got.DefinitionVersion != 3 || got.DefinitionRevision != "cafebabedeadbeefcafebabedeadbeef" {
		t.Fatalf("definition identity: %q %d %q", got.DefinitionName, got.DefinitionVersion, got.DefinitionRevision)
	}
	// Проход ещё не открыт: commit у роя нет.
	if got.Source == nil || *got.Source != (PipelineSource{Origin: source}) {
		t.Fatalf("source snapshot: %+v", got.Source)
	}
	// Повторная привязка, в том числе с другим источником — отказ.
	if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 3, "cafebabedeadbeefcafebabedeadbeef", source); err == nil {
		t.Fatal("second graph attached")
	}
	if err := s.AttachSwarmGraph(ctx, "missing", []byte(actorStartGraph), "release", 1, "rev", source); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing swarm: %v", err)
	}
	for _, tc := range []struct {
		name    string
		defName string
		defVer  int64
		defRev  string
		source  string
	}{
		{"empty definition name", "", 1, "rev", source},
		{"zero version", "release", 0, "rev", source},
		{"no origin", "release", 1, "rev", ""},
	} {
		if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), tc.defName, tc.defVer, tc.defRev, tc.source); err == nil {
			t.Errorf("%s accepted", tc.name)
		}
	}
}

// TestSwarmGraphImmutable: снимок графа и личность определения неизменяемы
// после привязки — прямой SQL-запись ловят триггеры реестра (#206).
func TestSwarmGraphImmutable(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", "https://git.example.org/repo"); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE swarms SET graph=x'00' WHERE id='s'`,
		`UPDATE swarms SET graph=NULL WHERE id='s'`,
		`UPDATE swarms SET definition_name='other' WHERE id='s'`,
		`UPDATE swarms SET definition_version=2 WHERE id='s'`,
		`UPDATE swarms SET definition_revision='other' WHERE id='s'`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("graph snapshot was mutable through %s", statement)
		}
	}
	got, err := s.GetSwarm(ctx, "s")
	if err != nil || string(got.Graph) != actorStartGraph || got.DefinitionName != "release" {
		t.Fatalf("graph survived: %+v %v", got, err)
	}
}

// TestPipelineSourceSnapshot: origin источника неизменяем на ключе роя,
// включая удаление роя — оно запрещено, пока снимок жив (#206).
func TestPipelineSourceSnapshot(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", "https://git.example.org/team/repo.git"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSwarm(ctx, "s")
	if err != nil || got.Source == nil || got.Source.Origin != "https://git.example.org/team/repo.git" {
		t.Fatalf("snapshot: %+v %v", got.Source, err)
	}
	for _, recursive := range []int{0, 1} {
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA recursive_triggers=%d`, recursive)); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`UPDATE pipeline_sources SET origin='https://changed.example.org/repo' WHERE swarm_id='s'`,
			`DELETE FROM pipeline_sources WHERE swarm_id='s'`,
			`INSERT OR REPLACE INTO pipeline_sources(swarm_id,origin) VALUES ('s','https://changed.example.org/repo')`,
			`INSERT OR REPLACE INTO swarms(id,project_id,owner_id,name,atespace_name,created_at,updated_at,graph) VALUES ('s','p','u','swarm','space','now','now',x'00')`,
		} {
			if _, err := s.db.Exec(statement); err == nil {
				t.Fatalf("source snapshot was mutable through %s", statement)
			}
		}
	}
}

// TestSwarmGraphSurvivesDefinitionDeletion: удаление определения из каталога
// не меняет и не обнуляет снимок и записанную личность определения у роя (#206).
func TestSwarmGraphSurvivesDefinitionDeletion(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	yes := func(context.Context) bool { return true }
	definition := strings.Replace(actorStartGraph, "name: actor-start", "name: release", 1)
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"release": definition}, yes); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetWorkflowDefinition(ctx, "p", "release")
	if err != nil {
		t.Fatal(err)
	}
	// Пока рой несёт снимок — удаление определения запрещено.
	if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", "https://git.example.org/repo"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkflowDefinition(ctx, "p", "admin", "release", yes); err == nil {
		t.Fatal("deleted definition while its snapshot lives on a swarm")
	}
	// Личность определена значениями: строку определения удаляем напрямую,
	// как это сделала бы будущая политика удаления, снимок должен уцелеть.
	if _, err := s.db.Exec(`DELETE FROM workflow_definitions WHERE project_id='p' AND name='release'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSwarm(ctx, "s")
	if err != nil || got.Graph == nil || string(got.Graph) != actorStartGraph {
		t.Fatalf("snapshot changed after definition deletion: %+v %v", got, err)
	}
	if got.DefinitionName != "release" || got.DefinitionVersion != stored.Version || got.DefinitionRevision != "rev" {
		t.Fatalf("definition identity changed: %q %d %q", got.DefinitionName, got.DefinitionVersion, got.DefinitionRevision)
	}
}

func TestPipelineSourceInsertRollback(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_pipeline_source BEFORE INSERT ON pipeline_sources BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", "https://git.example.org/repo")
	if err == nil {
		t.Fatal("source insert unexpectedly succeeded")
	}
	got, err := s.GetSwarm(ctx, "s")
	if err != nil || got.Graph != nil {
		t.Fatalf("partial attach persisted: %+v %v", got, err)
	}
}

func TestPipelineSourceNestedInsertRollback(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_pipeline_source BEFORE INSERT ON pipeline_sources BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", "https://git.example.org/repo")
		if err == nil {
			return errors.New("source insert unexpectedly succeeded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSwarm(ctx, "s")
	if err != nil || got.Graph != nil {
		t.Fatalf("partial attach persisted after outer commit: %+v %v", got, err)
	}
}

func TestPipelineSourceValidationAndNoPartialAttach(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	for i, origin := range []string{
		"",
		"https://user:secret@git.example.org/repo",
		"not a URL",
	} {
		if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", origin); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("case %d: %v", i, err)
		}
		got, err := s.GetSwarm(ctx, "s")
		if err != nil || got.Graph != nil {
			t.Fatalf("case %d attached graph: %v", i, err)
		}
	}
}

func TestBoundSwarmRefusesActorEffects(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if err := s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", "https://git.example.org/repo"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSwarmExecution(ctx, "s", "running"); err != nil {
		t.Fatal(err)
	}
	visit := PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "uid", ActorIncarnation: 1, RequestID: "request"}
	if _, err := s.AppendPipelineVisit(ctx, "s", "s", visit); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("inflight: %v", err)
	}
	visits, err := s.SwarmSteps(ctx, "s")
	if err != nil || len(visits) != 0 {
		t.Fatalf("unexpected visit: %+v %v", visits, err)
	}
	// Резерв попытки без подтверждения workspace (#212) исход не открывает:
	// ErrPipelineSourceUnverified снимает только запись подтверждения шага.
	if _, err := s.db.Exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,actor_uid,actor_incarnation,request_id) VALUES ('s',1,'actor','attesting','uid',1,'request')`); err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePipelineActorVisit(ctx, "s", "s", 1, "request", "uid", 1, "success", ""); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("completion: %v", err)
	}
	if visits, err := s.SwarmSteps(ctx, "s"); err != nil || len(visits) != 1 || visits[0].State != "attesting" {
		t.Fatalf("unverified completion changed the step: %+v %v", visits, err)
	}
}

// TestSwarmExecutionSeparateFromState: исполнение и инфраструктурное
// состояние роя — разные поля; остановка графа не выглядит удалением
// роя, а provisioning — запуском (#206). Падает, если значения начнут
// писаться в одно поле или пересекаться словарём.
func TestSwarmExecutionSeparateFromState(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	if _, err := s.db.Exec(`UPDATE swarms SET execution='running' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"provisioning", "active", "deleting", "error", "deleted"} {
		if _, err := s.db.Exec(`UPDATE swarms SET state=? WHERE id='s'`, state); err != nil {
			t.Fatalf("state %q: %v", state, err)
		}
		// Значение state не является значением execution и наоборот.
		if _, err := s.db.Exec(`UPDATE swarms SET execution=? WHERE id='s'`, state); err == nil {
			t.Fatalf("state value %q accepted as execution", state)
		}
	}
	if _, err := s.db.Exec(`UPDATE swarms SET execution='invented',state='active' WHERE id='s'`); err == nil {
		t.Fatal("invented execution accepted")
	}
	if _, err := s.db.Exec(`UPDATE swarms SET state='active' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSwarm(ctx, "s")
	if err != nil || got.State != "active" {
		t.Fatalf("state: %q %v", got.State, err)
	}
	if err := s.SetSwarmExecution(ctx, "s", "stopped"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSwarm(ctx, "s")
	if err != nil || got.Execution != "stopped" || got.State != "active" {
		t.Fatalf("execution stop changed infrastructure state: %+v %v", got, err)
	}
	if err := s.SetSwarmExecution(ctx, "s", "running"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSwarmState(ctx, "s", "deleting"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSwarm(ctx, "s")
	if err != nil || got.Execution != "running" || got.State != "deleting" {
		t.Fatalf("state change clobbered execution: %+v %v", got, err)
	}
}

// TestSwarmStepTimestamps: started_at проставляется при начале шага,
// finished_at — при завершении (#206). Падает, если поля перестанут
// заполняться.
func TestSwarmStepTimestamps(t *testing.T) {
	ctx := context.Background()
	s := swarmTestStore(t, "r")
	if err := attachTestGraph(ctx, s, "r", runGraph); err != nil {
		t.Fatal(err)
	}
	roleActor(t, s, "r", "a", "build")
	if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	visits, err := s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 1 {
		t.Fatalf("steps: %+v %v", visits, err)
	}
	if visits[0].StartedAt == nil || *visits[0].StartedAt == "" {
		t.Fatalf("waiting step has no started_at: %+v", visits[0])
	}
	if visits[0].FinishedAt != nil {
		t.Fatalf("unfinished step has finished_at: %+v", visits[0])
	}
	if err := s.AnswerPipelineVisit(ctx, "r", "r", 1, "success"); err != nil {
		t.Fatal(err)
	}
	visits, err = s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 2 {
		t.Fatalf("steps after answer: %+v %v", visits, err)
	}
	if visits[0].FinishedAt == nil || *visits[0].FinishedAt == "" {
		t.Fatalf("answered step has no finished_at: %+v", visits[0])
	}
	// Актёрный шаг: started_at в момент перевода в inflight.
	v, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "req"})
	if err != nil {
		t.Fatal(err)
	}
	visits, err = s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 2 || visits[1].StartedAt == nil || *visits[1].StartedAt == "" {
		t.Fatalf("inflight step has no started_at: %+v %v", visits, err)
	}
	if err := s.CompletePipelineActorVisit(ctx, "r", "r", v.Sequence, "req", "a", 1, "success", ""); err != nil {
		t.Fatal(err)
	}
	visits, err = s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 3 {
		t.Fatalf("steps after completion: %+v %v", visits, err)
	}
	if visits[1].FinishedAt == nil || *visits[1].FinishedAt == "" {
		t.Fatalf("completed step has no finished_at: %+v", visits[1])
	}
}

// TestPipelineSourceForeignVersionRefused: миграций нет (#205); база
// версии 14 отвергается без изменения схемы и версии, триггеры
// неизменности при этом не пересоздаются.
func TestPipelineSourceForeignVersionRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'); INSERT INTO swarm_members VALUES ('s','u','owner')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version=14`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, s.db)
	s.Close()
	if reopened, err := Open(ctx, path); err == nil {
		reopened.Close()
		t.Fatal("previous pipeline source version accepted")
	} else if !strings.Contains(err.Error(), "incompatible database schema") {
		t.Fatal(err)
	}
	if err := assertDatabaseUnchanged(t, path, 14, before); err != nil {
		t.Fatal(err)
	}
}
