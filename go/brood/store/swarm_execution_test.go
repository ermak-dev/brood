package store

// FEATURE: Запуск и остановка роя (#211).
// SPEC: Запуск в одной транзакции проверяет рой, права, исполнение,
// определение каталога проекта, граф валидатором исполнителя, источник и
// привязки ролей (#221), затем привязывает снимок с личностью определения,
// переводит исполнение в running/waiting и создаёт первый шаг стартового
// узла. Любой отказ — до записи и с причиной. Остановка допустима только из
// running/waiting, замораживает проход и не трогает акторов, workspace и
// привязки. Повторный запуск — новый проход на том же снимке: шаги прошлых
// проходов не меняются, нумерация продолжается.
// IMPLEMENTATION: StartSwarm/StopSwarm поверх AttachSwarmGraph,
// CheckSwarmRolesFit и pipeline.Begin; снимок графа один на рой (#206).
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	startOrigin = "https://git.example.org/team/repo.git"
	startCommit = "0123456789abcdef0123456789abcdef01234567"
	// startGoal — задача прохода, с которой стартуют проходы фикстур (#242).
	startGoal = "Ship issue #7: the parser drops trailing comments."
)

// startRelease — человек решает, затем актор роли build.
const startRelease = `apiVersion: brood.dev/v1alpha1
name: release
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Ship?
    options:
      - {label: Ship, outcome: success}
      - {label: Hold, outcome: fail}
  build: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: ask, to: build, label: Ship}
  - {from: ask, to: done, label: Hold}
  - {from: build, to: done}
goalGates: [done]
`

// startApprove — только оператор: проход завершается его ответом.
const startApprove = `apiVersion: brood.dev/v1alpha1
name: approve
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Approve?
    options:
      - {label: Approve, outcome: success}
  done: {type: exit}
edges:
  - {from: ask, to: done}
goalGates: [done]
`

// startBuildFirst — проход начинается с актора.
const startBuildFirst = `apiVersion: brood.dev/v1alpha1
name: build-first
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: build, to: done}
goalGates: [done]
`

// startStore — рой «one» проекта «one» активен, его актор a работает в пуле
// и привязан к роли build; у проекта есть repository и каталог определений.
// u — owner роя и участник проекта, v — viewer, root — администратор,
// публикующий каталог. У проекта «two» своё определение foreign.
func startStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	seedUser(t, s, "v", "viewer")
	seedUser(t, s, "root", "root")
	mustNil(t, s.SetUserAdmin(ctx, "root", true))
	mustNil(t, s.AddMember(ctx, "one", "u"))
	mustNil(t, s.AddMember(ctx, "one", "v"))
	mustNil(t, s.SetSwarmMember(ctx, "one", "v", "viewer"))
	mustNil(t, s.SetProjectRepository(ctx, "one", startOrigin))
	if _, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 201)); err != nil {
		t.Fatal(err)
	}
	setActor(t, s, "a", "phase='running'")
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "a"}))
	publish(t, s, "one", map[string]string{"release": startRelease, "approve": startApprove, "build-first": startBuildFirst})
	publish(t, s, "two", map[string]string{"foreign": strings.Replace(startApprove, "name: approve", "name: foreign", 1)})
	return s
}

// publish публикует определения в каталог проекта от имени администратора.
func publish(t *testing.T, s *Store, project string, definitions map[string]string) {
	t.Helper()
	if _, err := s.SyncWorkflowDefinitions(context.Background(), project, "root", definitions, allow); err != nil {
		t.Fatal(err)
	}
}

func startOne(s *Store, definition string) (Swarm, error) {
	return s.StartSwarm(context.Background(), SwarmStart{SwarmID: "one", UserID: "u", Definition: definition, Commit: startCommit, Goal: startGoal}, allow)
}

// dumpTables печатает строки таблиц целиком в стабильном порядке: сравнение
// дампов до и после операции доказывает, что она ничего не записала.
func dumpTables(t *testing.T, s *Store, tables ...string) string {
	t.Helper()
	var out strings.Builder
	for _, table := range tables {
		rows, err := s.db.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			lines = append(lines, fmt.Sprint(values...))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(lines)
		fmt.Fprintf(&out, "%s:\n%s\n", table, strings.Join(lines, "\n"))
	}
	return out.String()
}

// executionTables — всё, что запуск и остановка могут записать.
var executionTables = []string{"swarms", "swarm_steps", "swarm_requests", "pipeline_sources"}

// lifeTables — акторы, их workspace-claims, привязки и ACL: их не трогают
// ни запуск, ни остановка.
var lifeTables = []string{"managed_actors", "coordination_claims", "swarm_role_bindings", "swarm_members", "managed_pools"}

func openNodeRequestOf(t *testing.T, s *Store, sequence int64) SwarmRequest {
	t.Helper()
	requests, err := s.SwarmRequests(context.Background(), "one")
	mustNil(t, err)
	for _, r := range requests {
		if r.Origin == "node" && r.Sequence != nil && *r.Sequence == sequence {
			return r
		}
	}
	t.Fatalf("no node request for step %d: %+v", sequence, requests)
	return SwarmRequest{}
}

func TestStartSwarmFirstPass(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	life := dumpTables(t, s, lifeTables...)
	definition, err := s.GetWorkflowDefinition(ctx, "one", "release")
	mustNil(t, err)
	w, err := startOne(s, "release")
	mustNil(t, err)
	if w.Execution != "waiting" || w.State != "active" || string(w.Graph) != startRelease {
		t.Fatalf("started swarm: execution=%q state=%q graph=%q", w.Execution, w.State, w.Graph)
	}
	if w.DefinitionName != "release" || w.DefinitionVersion != definition.Version || w.DefinitionRevision != definition.Revision {
		t.Fatalf("definition identity %q %d %q, catalog %d %q", w.DefinitionName, w.DefinitionVersion, w.DefinitionRevision, definition.Version, definition.Revision)
	}
	if w.Source == nil || *w.Source != (PipelineSource{Origin: startOrigin, Commit: startCommit}) {
		t.Fatalf("source snapshot: %+v", w.Source)
	}
	steps, err := s.SwarmSteps(ctx, "one")
	mustNil(t, err)
	if len(steps) != 1 || steps[0].Sequence != 1 || steps[0].NodeID != "ask" || steps[0].State != "waiting" {
		t.Fatalf("first step: %+v", steps)
	}
	if r := openNodeRequestOf(t, s, 1); r.State != "open" || r.Question != "Ship?" {
		t.Fatalf("node request: %+v", r)
	}
	// Запуск не создаёт акторов и не трогает их, их workspace и привязки.
	if after := dumpTables(t, s, lifeTables...); after != life {
		t.Fatalf("start changed actors or bindings:\n%s\nwant\n%s", after, life)
	}
}

// TestStartSwarmResolvedOrigin — commit, разрешённый сервером из repository
// проекта до транзакции (#228), пиннится, пока repository прежний.
func TestStartSwarmResolvedOrigin(t *testing.T) {
	s := startStore(t)
	w, err := s.StartSwarm(context.Background(), SwarmStart{SwarmID: "one", UserID: "u", Definition: "release", Commit: startCommit, Origin: startOrigin, Goal: startGoal}, allow)
	mustNil(t, err)
	if w.Source == nil || *w.Source != (PipelineSource{Origin: startOrigin, Commit: startCommit}) {
		t.Fatalf("source snapshot: %+v", w.Source)
	}
}

func TestStartSwarmActorStartNode(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	w, err := startOne(s, "build-first")
	mustNil(t, err)
	steps, err := s.SwarmSteps(ctx, "one")
	mustNil(t, err)
	if w.Execution != "running" || len(steps) != 1 || steps[0].NodeID != "build" || steps[0].State != "pending" || steps[0].ActorUID != "" {
		t.Fatalf("actor start: execution=%q steps=%+v", w.Execution, steps)
	}
	requests, err := s.SwarmRequests(ctx, "one")
	if err != nil || len(requests) != 0 {
		t.Fatalf("actor start opened requests: %+v %v", requests, err)
	}
}

// TestStartSwarmRefusesBeforeWrite — каждый отказ называет причину и ничего
// не пишет: дамп таблиц исполнения и жизни акторов не меняется.
func TestStartSwarmRefusesBeforeWrite(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Store)
		in    SwarmStart
		deny  bool
		want  error
		text  string
	}{
		{name: "swarm not active", setup: func(t *testing.T, s *Store) { mustNil(t, s.SetSwarmState(ctx, "one", "error")) }, want: ErrSwarmExecutionState, text: "swarm is error"},
		{name: "already waiting", setup: func(t *testing.T, s *Store) { mustStart(t, s, "release") }, want: ErrSwarmExecutionState, text: "execution is waiting"},
		{name: "already running", setup: func(t *testing.T, s *Store) { mustStart(t, s, "build-first") }, in: SwarmStart{Definition: "build-first"}, want: ErrSwarmExecutionState, text: "execution is running"},
		{name: "unbound role", setup: func(t *testing.T, s *Store) { execFit(t, s, `DELETE FROM swarm_role_bindings`) }, want: ErrSwarmRoleUnbound, text: `role "build"`},
		{name: "actor provisioning", setup: func(t *testing.T, s *Store) { setActor(t, s, "a", "phase='provisioning'") }, want: ErrSwarmRoleInactive, text: "provisioning"},
		{name: "actor stopped", setup: func(t *testing.T, s *Store) { setActor(t, s, "a", "phase='stopped', desired='stopped'") }, want: ErrSwarmRoleInactive, text: "stopped"},
		{name: "pool draining", setup: func(t *testing.T, s *Store) { execFit(t, s, `UPDATE managed_pools SET state='draining'`) }, want: ErrPoolUnavailable, text: `pool "pve"`},
		{name: "definition missing", in: SwarmStart{Definition: "absent"}, want: ErrWorkflowDefinitionMissing, text: `"absent"`},
		{name: "definition of another project", in: SwarmStart{Definition: "foreign"}, want: ErrWorkflowDefinitionMissing, text: `"foreign"`},
		{name: "graph with retries", setup: func(t *testing.T, s *Store) {
			publish(t, s, "one", map[string]string{"release": startRelease + "retry: {maxRetries: 2}\n"})
		}, want: ErrWorkflowGraphInvalid, text: "retries"},
		{name: "goal gate before exit", setup: func(t *testing.T, s *Store) {
			publish(t, s, "one", map[string]string{"release": strings.Replace(startRelease, "goalGates: [done]", "goalGates: [build]", 1)})
		}, want: ErrWorkflowGraphInvalid, text: "goal gates"},
		{name: "corrupt catalog row", setup: func(t *testing.T, s *Store) {
			execFit(t, s, `UPDATE workflow_definitions SET yaml='nodes: [' WHERE name='release'`)
		}, want: ErrWorkflowGraphInvalid, text: "release"},
		{name: "snapshot without source", setup: func(t *testing.T, s *Store) {
			_, err := s.db.Exec(`UPDATE swarms SET graph=?,execution='stopped',definition_name='release',definition_version=1,definition_revision='r' WHERE id='one'`, []byte(startRelease))
			mustNil(t, err)
		}, want: ErrSwarmSourceInvalid, text: "no source snapshot"},
		{name: "project without repository", setup: func(t *testing.T, s *Store) { mustNil(t, s.SetProjectRepository(ctx, "one", "")) }, want: ErrSwarmSourceInvalid, text: "repository"},
		{name: "commit missing", in: SwarmStart{Commit: "-"}, want: ErrSwarmSourceInvalid, text: "commit"},
		{name: "commit abbreviated", in: SwarmStart{Commit: "0123456"}, want: ErrSwarmSourceInvalid, text: "commit"},
		{name: "repository changed after the commit was resolved", in: SwarmStart{Origin: "https://git.example.org/team/other.git"}, want: ErrSwarmSourceChanged, text: "repository changed"},
		{name: "repository cleared after the commit was resolved", setup: func(t *testing.T, s *Store) { mustNil(t, s.SetProjectRepository(ctx, "one", "")) }, in: SwarmStart{Origin: startOrigin}, want: ErrSwarmSourceInvalid, text: "repository"},
		{name: "session revoked", deny: true, want: ErrSwarmRequestDenied},
		{name: "viewer", in: SwarmStart{UserID: "v"}, want: ErrSwarmRequestDenied},
		{name: "missing swarm", in: SwarmStart{SwarmID: "absent"}, want: ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startStore(t)
			if tc.setup != nil {
				tc.setup(t, s)
			}
			in := SwarmStart{SwarmID: "one", UserID: "u", Definition: "release", Commit: startCommit, Goal: startGoal}
			if tc.in.SwarmID != "" {
				in.SwarmID = tc.in.SwarmID
			}
			if tc.in.UserID != "" {
				in.UserID = tc.in.UserID
			}
			if tc.in.Definition != "" {
				in.Definition = tc.in.Definition
			}
			in.Origin = tc.in.Origin
			switch tc.in.Commit {
			case "":
			case "-":
				in.Commit = ""
			default:
				in.Commit = tc.in.Commit
			}
			authorize := allow
			if tc.deny {
				authorize = func(context.Context) bool { return false }
			}
			before := dumpTables(t, s, append(executionTables, lifeTables...)...)
			_, err := s.StartSwarm(ctx, in, authorize)
			if !errors.Is(err, tc.want) || !strings.Contains(fmt.Sprint(err), tc.text) {
				t.Fatalf("start: %v, want %v naming %q", err, tc.want, tc.text)
			}
			if after := dumpTables(t, s, append(executionTables, lifeTables...)...); after != before {
				t.Fatalf("refused start wrote:\n%s\nwant\n%s", after, before)
			}
		})
	}
}

// TestStartSwarmWaitingStop — запуск отказывает, пока reset или update ещё
// только должны остановить работающего актора роли (#275): иначе проход
// начался бы, а его шаг получил бы отказ. Отказ называет роль и ждущую
// остановку и ничего не пишет; тот же актор без незавершённого изменения
// запуск проходит.
func TestStartSwarmWaitingStop(t *testing.T) {
	for change, waiting := range waitingStops {
		t.Run(change, func(t *testing.T) {
			s := startStore(t)
			setActor(t, s, "a", waiting)
			before := dumpTables(t, s, append(executionTables, lifeTables...)...)
			_, err := startOne(s, "release")
			waitingStopRefused(t, "start", change, err)
			if !strings.Contains(fmt.Sprint(err), `role "build": `) {
				t.Fatalf("start refusal does not name the role: %v", err)
			}
			if after := dumpTables(t, s, append(executionTables, lifeTables...)...); after != before {
				t.Fatalf("refused start wrote:\n%s\nwant\n%s", after, before)
			}
			setActor(t, s, "a", settledRunning)
			mustStart(t, s, "release")
		})
	}
}

func mustStart(t *testing.T, s *Store, definition string) Swarm {
	t.Helper()
	w, err := startOne(s, definition)
	mustNil(t, err)
	return w
}

// TestStartSwarmConcurrent — одновременные запуски, в том числе разных
// определений, дают одно исполнение, один снимок и один первый шаг.
func TestStartSwarmConcurrent(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	const starters = 16
	errs := make([]error, starters)
	var ready, done sync.WaitGroup
	ready.Add(starters)
	done.Add(starters)
	gate := make(chan struct{})
	for i := range starters {
		definition := "release"
		if i%2 == 1 {
			definition = "build-first"
		}
		go func() {
			defer done.Done()
			ready.Done()
			<-gate
			_, errs[i] = startOne(s, definition)
		}()
	}
	ready.Wait()
	close(gate)
	done.Wait()
	started := 0
	for _, err := range errs {
		switch {
		case err == nil:
			started++
		case !errors.Is(err, ErrSwarmExecutionState):
			t.Fatalf("losing start: %v", err)
		}
	}
	if started != 1 {
		t.Fatalf("%d concurrent starts succeeded, want 1", started)
	}
	steps, err := s.SwarmSteps(ctx, "one")
	mustNil(t, err)
	var sources int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM pipeline_sources WHERE swarm_id='one'`).Scan(&sources))
	if len(steps) != 1 || steps[0].Sequence != 1 || sources != 1 {
		t.Fatalf("concurrent starts left steps %+v and %d sources", steps, sources)
	}
}

func TestStopSwarmFreezesPass(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	mustStart(t, s, "release")
	request := openNodeRequestOf(t, s, 1)
	life := dumpTables(t, s, lifeTables...)
	w, err := s.StopSwarm(ctx, "one", "u", allow)
	mustNil(t, err)
	if w.Execution != "stopped" || w.State != "active" {
		t.Fatalf("stopped swarm: execution=%q state=%q", w.Execution, w.State)
	}
	steps, err := s.SwarmSteps(ctx, "one")
	mustNil(t, err)
	if len(steps) != 1 || steps[0].State != "stopped" || steps[0].FinishedAt == nil {
		t.Fatalf("stopped step: %+v", steps)
	}
	if r := openNodeRequestOf(t, s, 1); r.State != "cancelled" {
		t.Fatalf("node request after stop: %+v", r)
	}
	// Остановка не уничтожает актора, не трогает его workspace и не снимает
	// привязку роли.
	if after := dumpTables(t, s, lifeTables...); after != life {
		t.Fatalf("stop changed actors, workspace or bindings:\n%s\nwant\n%s", after, life)
	}
	// Ответ на запрос, открытый до остановки, граф не продвигает.
	frozen := dumpTables(t, s, executionTables...)
	if err := answer(s, request, "u", "success", ""); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("answer after stop: %v", err)
	}
	if after := dumpTables(t, s, executionTables...); after != frozen {
		t.Fatalf("answer after stop wrote:\n%s\nwant\n%s", after, frozen)
	}
}

// TestStopSwarmRefusesFinishedExecution — остановка не притворяется
// успехом: из idle, succeeded, failed и stopped — отказ с названием
// состояния, без записи.
func TestStopSwarmRefusesFinishedExecution(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, state string
		setup       func(*testing.T, *Store)
	}{
		{"idle", "idle", func(*testing.T, *Store) {}},
		{"succeeded", "succeeded", func(t *testing.T, s *Store) {
			mustStart(t, s, "approve")
			mustNil(t, answer(s, openNodeRequestOf(t, s, 1), "u", "success", ""))
		}},
		{"failed", "failed", func(t *testing.T, s *Store) {
			mustStart(t, s, "release")
			mustNil(t, answer(s, openNodeRequestOf(t, s, 1), "u", "fail", ""))
		}},
		{"stopped", "stopped", func(t *testing.T, s *Store) {
			mustStart(t, s, "release")
			if _, err := s.StopSwarm(ctx, "one", "u", allow); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startStore(t)
			tc.setup(t, s)
			w, err := s.GetSwarm(ctx, "one")
			mustNil(t, err)
			if w.Execution != tc.state {
				t.Fatalf("fixture execution %q, want %q", w.Execution, tc.state)
			}
			before := dumpTables(t, s, append(executionTables, lifeTables...)...)
			_, err = s.StopSwarm(ctx, "one", "u", allow)
			if !errors.Is(err, ErrSwarmExecutionState) || !strings.Contains(err.Error(), "execution is "+tc.state) {
				t.Fatalf("stop from %s: %v", tc.state, err)
			}
			if after := dumpTables(t, s, append(executionTables, lifeTables...)...); after != before {
				t.Fatalf("refused stop wrote:\n%s\nwant\n%s", after, before)
			}
		})
	}
}

func TestStopSwarmRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		user   string
		deny   bool
		setup  func(*testing.T, *Store)
		want   error
		reason string
	}{
		{name: "swarm not active", user: "u", setup: func(t *testing.T, s *Store) { mustNil(t, s.SetSwarmState(ctx, "one", "error")) }, want: ErrSwarmExecutionState, reason: "swarm is error"},
		{name: "viewer", user: "v", want: ErrSwarmRequestDenied},
		{name: "session revoked", user: "u", deny: true, want: ErrSwarmRequestDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startStore(t)
			mustStart(t, s, "release")
			if tc.setup != nil {
				tc.setup(t, s)
			}
			authorize := allow
			if tc.deny {
				authorize = func(context.Context) bool { return false }
			}
			before := dumpTables(t, s, executionTables...)
			if _, err := s.StopSwarm(ctx, "one", tc.user, authorize); !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("stop: %v, want %v naming %q", err, tc.want, tc.reason)
			}
			if after := dumpTables(t, s, executionTables...); after != before {
				t.Fatalf("refused stop wrote:\n%s\nwant\n%s", after, before)
			}
		})
	}
}

// stepRows — строки шагов до данного номера включительно, целиком.
func stepRows(t *testing.T, s *Store, through int64) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT * FROM swarm_steps WHERE swarm_id='one' AND sequence<=? ORDER BY sequence`, through)
	mustNil(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	mustNil(t, err)
	var out strings.Builder
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		mustNil(t, rows.Scan(pointers...))
		fmt.Fprintln(&out, values)
	}
	mustNil(t, rows.Err())
	return out.String()
}

// TestRestartIsNewPass — повторный запуск начинает новый проход на снимке
// первого: шаги прошлых проходов и снимок не меняются, нумерация
// продолжается, новая версия в каталоге снимок роя не подменяет. Сменить
// определение повторным запуском нельзя; commit — свой у каждого прохода
// (#229, swarm_pass_source_test.go).
func TestRestartIsNewPass(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	first := mustStart(t, s, "release")
	mustNil(t, answer(s, openNodeRequestOf(t, s, 1), "u", "success", ""))
	stopOne(t, s)
	steps, err := s.SwarmSteps(ctx, "one")
	mustNil(t, err)
	if len(steps) != 2 || steps[1].NodeID != "build" || steps[1].State != "stopped" {
		t.Fatalf("first pass after stop: %+v", steps)
	}
	history := stepRows(t, s, 2)
	publish(t, s, "one", map[string]string{"release": strings.Replace(startRelease, "Ship?", "Ship now?", 1)})

	for _, tc := range []struct {
		name string
		in   SwarmStart
		text string
	}{
		{"another definition", SwarmStart{Definition: "approve", Commit: startCommit}, `"release"`},
	} {
		before := dumpTables(t, s, executionTables...)
		in := tc.in
		in.SwarmID, in.UserID, in.Goal = "one", "u", startGoal
		if _, err := s.StartSwarm(ctx, in, allow); !errors.Is(err, ErrSwarmGraphPinned) || !strings.Contains(err.Error(), tc.text) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if after := dumpTables(t, s, executionTables...); after != before {
			t.Fatalf("%s wrote:\n%s\nwant\n%s", tc.name, after, before)
		}
	}

	// Повторный запуск с тем же commit — новый проход на том же источнике.
	again, err := s.StartSwarm(ctx, SwarmStart{SwarmID: "one", UserID: "u", Definition: "release", Commit: startCommit, Goal: startGoal}, allow)
	mustNil(t, err)
	if string(again.Graph) != string(first.Graph) || again.DefinitionVersion != first.DefinitionVersion || again.DefinitionRevision != first.DefinitionRevision || *again.Source != *first.Source {
		t.Fatalf("restart replaced the snapshot: %+v, was %+v", again, first)
	}
	if again.Execution != "waiting" {
		t.Fatalf("restart execution %q", again.Execution)
	}
	if got := stepRows(t, s, 2); got != history {
		t.Fatalf("restart changed the previous pass:\n%s\nwant\n%s", got, history)
	}
	steps, err = s.SwarmSteps(ctx, "one")
	mustNil(t, err)
	if len(steps) != 3 || steps[2].Sequence != 3 || steps[2].NodeID != "ask" || steps[2].State != "waiting" {
		t.Fatalf("second pass: %+v", steps)
	}
	if r := openNodeRequestOf(t, s, 3); r.Question != "Ship?" || r.State != "open" {
		t.Fatalf("second pass asks from the catalog, not the snapshot: %+v", r)
	}
	stopOne(t, s)
	history = stepRows(t, s, 3)
	mustStart(t, s, "release")
	if got := stepRows(t, s, 3); got != history {
		t.Fatalf("third pass changed earlier passes:\n%s\nwant\n%s", got, history)
	}
}

// TestRestartKeepsStoppedRequestClosed — ответ на запрос, открытый до
// остановки, не продвигает ни остановленный проход, ни следующий за ним.
func TestRestartKeepsStoppedRequestClosed(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	mustStart(t, s, "release")
	stale := openNodeRequestOf(t, s, 1)
	stopOne(t, s)
	mustStart(t, s, "release")
	before := dumpTables(t, s, executionTables...)
	if err := answer(s, stale, "u", "success", ""); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("answer to the stopped pass request: %v", err)
	}
	if after := dumpTables(t, s, executionTables...); after != before {
		t.Fatalf("stale answer wrote:\n%s\nwant\n%s", after, before)
	}
	if r := openNodeRequestOf(t, s, 1); r.State != "cancelled" {
		t.Fatalf("stopped pass request: %+v", r)
	}
	mustNil(t, answer(s, openNodeRequestOf(t, s, 2), "u", "fail", ""))
	w, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	if w.Execution != "failed" {
		t.Fatalf("new pass did not advance on its own request: %q", w.Execution)
	}
}

func stopOne(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.StopSwarm(context.Background(), "one", "u", allow); err != nil {
		t.Fatal(err)
	}
}

// TestRestartRechecksRoles — повторный запуск сверяет привязки заново: актор,
// переставший работать после первого прохода, не берёт новый проход.
func TestRestartRechecksRoles(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	mustStart(t, s, "build-first")
	if _, err := s.StopSwarm(ctx, "one", "u", allow); err != nil {
		t.Fatal(err)
	}
	setActor(t, s, "a", "phase='provisioning'")
	before := dumpTables(t, s, executionTables...)
	if _, err := startOne(s, "build-first"); !errors.Is(err, ErrSwarmRoleInactive) {
		t.Fatalf("restart with a provisioning actor: %v", err)
	}
	if after := dumpTables(t, s, executionTables...); after != before {
		t.Fatalf("refused restart wrote:\n%s\nwant\n%s", after, before)
	}
}
