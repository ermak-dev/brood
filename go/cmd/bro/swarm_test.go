package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
)

const swarmCLIRoles = `{"swarmId":"s3","execution":"idle","graph":{"pinned":false,"name":"release","version":3,"revision":"rev"},"roles":[{"role":"build","inGraph":true,"state":"ready","actorUid":"u1","actorName":"master","actorIncarnation":2},{"role":"extra","inGraph":false,"state":"missing","actorUid":"gone","actorIncarnation":1},{"role":"review","inGraph":true,"state":"unbound"}]}`

// FEATURE: bro swarm roles, bind and unbind (#235).
// SPEC: The swarm is named by ID or by a name unique among accessible
// swarms, the actor by managed name or UID of that swarm; bind sends the UID
// and the incarnation it resolved; arity errors never make a request.
// IMPLEMENTATION: Session client, swarm and actor resolution, tab output.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSwarmRolesCLI(t *testing.T) {
	var mutations []string
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"},{"id":"p2","name":"other"}]`)
		case r.URL.Path == "/api/projects/p1/swarms":
			fmt.Fprint(w, `[{"id":"s1","name":"main","state":"active"},{"id":"s9","name":"solo","state":"deleted"}]`)
		case r.URL.Path == "/api/projects/p2/swarms":
			fmt.Fprint(w, `[{"id":"s2","name":"main","state":"active"},{"id":"s3","name":"solo","state":"active"}]`)
		case r.URL.Path == "/api/swarms/s3/actors":
			fmt.Fprint(w, `{"results":[{"metadata":{"name":"native","uid":"n1"}},{"metadata":{"name":"master","uid":"u1"},"provider":"proxmox","generation":"2","phase":"running"}]}`)
		case r.URL.Path == "/api/swarms/s3/roles" && r.Method == "GET":
			if got := r.URL.Query().Get("definition"); got != "release" && got != "" {
				t.Errorf("definition %q", got)
			}
			fmt.Fprint(w, swarmCLIRoles)
		case r.URL.Path == "/api/swarms/s3/roles/build" && r.Method == "PUT":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if !reflect.DeepEqual(body, map[string]any{"actorUid": "u1", "actorIncarnation": float64(2)}) {
				t.Errorf("bind body %v", body)
			}
			mutations = append(mutations, "PUT build")
			fmt.Fprint(w, swarmCLIRoles)
		case r.URL.Path == "/api/swarms/s3/roles/review" && r.Method == "DELETE":
			mutations = append(mutations, "DELETE review")
			fmt.Fprint(w, swarmCLIRoles)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	table := "swarm s3 (solo): execution idle\n" +
		"graph: release version 3 revision rev (catalog)\n" +
		"ROLE\tGRAPH\tSTATE\tACTOR\tUID\tINCARNATION\n" +
		"build\tyes\tready\tmaster\tu1\t2\n" +
		"extra\tno\tmissing\t-\tgone\t1\n" +
		"review\tyes\tunbound\t-\t-\t-\n"
	for _, args := range [][]string{
		{"swarm", "roles", "solo", "--definition", "release"},
		{"swarm", "roles", "s3", "--definition=release"},
	} {
		code, out, diag := invoke(t, "", args...)
		if code != 0 || out != table || diag != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
	for _, actor := range []string{"master", "u1"} {
		code, out, diag := invoke(t, "", "swarm", "bind", "solo", "build", actor)
		if code != 0 || out != "bound build to master (uid u1, incarnation 2): ready\n" || diag != "" {
			t.Fatalf("bind %s: %d %q %q", actor, code, out, diag)
		}
	}
	code, out, diag := invoke(t, "", "swarm", "unbind", "s3", "review")
	if code != 0 || out != "unbound review\n" || diag != "" {
		t.Fatalf("unbind: %d %q %q", code, out, diag)
	}
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{"swarm", "roles", "main"}, "swarm name is ambiguous; use its ID"},
		{[]string{"swarm", "roles", "absent"}, "swarm not found or not accessible"},
		{[]string{"swarm", "bind", "s3", "build", "native"}, "actor not found in the swarm"},
		{[]string{"swarm", "bind", "s3", "build", "ghost"}, "actor not found in the swarm"},
	} {
		code, out, diag := invoke(t, "", tc.args...)
		if code != 1 || out != "" || !strings.Contains(diag, tc.reason) {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, diag)
		}
	}
	if !reflect.DeepEqual(mutations, []string{"PUT build", "PUT build", "DELETE review"}) {
		t.Fatalf("mutations: %v", mutations)
	}
}

// FEATURE: bro swarm start and stop (#242).
// SPEC: start sends the definition, the pass goal from --goal or from
// --goal-file (a file or - for stdin, one trailing newline dropped) and an
// optional commit; stop sends nothing. Both print the execution, its graph,
// source and last step. Invalid arguments and unreadable or oversized goal
// files never make a request.
// IMPLEMENTATION: parseSwarm, readGoalFile, clientapi StartSwarm/StopSwarm.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSwarmStartStopCLI(t *testing.T) {
	var starts []map[string]any
	stops := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"}]`)
		case r.URL.Path == "/api/projects/p1/swarms":
			fmt.Fprint(w, `[{"id":"s3","name":"solo","state":"active"}]`)
		case r.URL.Path == "/api/swarms/s3/start" && r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			starts = append(starts, body)
			fmt.Fprint(w, `{"swarmId":"s3","execution":"running","definition":{"name":"release","version":3,"revision":"rev"},"source":{"origin":"https://git.example.org/team/repo.git","commit":"0123456789abcdef0123456789abcdef01234567"},"goal":"Fix #42.","step":{"sequence":4,"nodeId":"build","state":"pending"}}`)
		case r.URL.Path == "/api/swarms/s3/stop" && r.Method == "POST":
			stops++
			fmt.Fprint(w, `{"swarmId":"s3","execution":"stopped","definition":{"name":"release","version":3,"revision":"rev"},"source":{"origin":"https://git.example.org/team/repo.git","commit":"0123456789abcdef0123456789abcdef01234567"},"goal":"Fix #42.","step":{"sequence":4,"nodeId":"build","state":"unconfirmed","detail":"the swarm stopped during instruction delivery; the delivery outcome is unknown"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	started := "started swarm s3 (solo): execution running\n" +
		"graph: release version 3 revision rev\n" +
		"source: https://git.example.org/team/repo.git commit 0123456789abcdef0123456789abcdef01234567\n" +
		"step 4 build: pending\n"
	goalFile := filepath.Join(t.TempDir(), "goal.md")
	if err := os.WriteFile(goalFile, []byte("Fix #42.\n\t- keep the API\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input string
		args  []string
		body  map[string]any
	}{
		{"", []string{"swarm", "start", "solo", "--definition", "release", "--goal", "Fix #42."}, map[string]any{"definition": "release", "goal": "Fix #42."}},
		{"", []string{"swarm", "start", "--definition=release", "--goal=-Fix #42.", "s3", "--commit", "abc"}, map[string]any{"definition": "release", "goal": "-Fix #42.", "commit": "abc"}},
		{"", []string{"swarm", "start", "s3", "--goal-file", goalFile, "--definition", "release"}, map[string]any{"definition": "release", "goal": "Fix #42.\n\t- keep the API"}},
		{"From stdin.\r\n", []string{"swarm", "start", "s3", "--definition", "release", "--goal-file", "-"}, map[string]any{"definition": "release", "goal": "From stdin."}},
	} {
		starts = nil
		code, out, diag := invoke(t, tc.input, tc.args...)
		if code != 0 || out != started || diag != "" {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, diag)
		}
		if len(starts) != 1 || !reflect.DeepEqual(starts[0], tc.body) {
			t.Fatalf("%v: start bodies %v, want %v", tc.args, starts, tc.body)
		}
	}
	code, out, diag := invoke(t, "", "swarm", "stop", "solo")
	want := "stopped swarm s3 (solo): execution stopped\n" +
		"graph: release version 3 revision rev\n" +
		"source: https://git.example.org/team/repo.git commit 0123456789abcdef0123456789abcdef01234567\n" +
		"step 4 build: unconfirmed (the swarm stopped during instruction delivery; the delivery outcome is unknown)\n"
	if code != 0 || out != want || diag != "" || stops != 1 {
		t.Fatalf("stop: %d %q %q, %d stops", code, out, diag, stops)
	}
}

// TestSwarmStartGoalFileRefusals — задача из файла, который не читается или
// длиннее предела сервера, отказывается до запроса.
func TestSwarmStartGoalFileRefusals(t *testing.T) {
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unreadable goal made a request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	dir := t.TempDir()
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", clientapi.MaxSwarmGoalBytes+1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input, file, reason string
	}{
		{"", filepath.Join(dir, "absent"), "could not open the goal file"},
		{"", dir, "could not read the goal file"},
		{"", large, "the goal file exceeds 4096 bytes"},
		{strings.Repeat("y", clientapi.MaxSwarmGoalBytes+1), "-", "the goal file exceeds 4096 bytes"},
	} {
		code, out, diag := invoke(t, tc.input, "swarm", "start", "s3", "--definition", "release", "--goal-file", tc.file)
		if code != 1 || out != "" || !strings.Contains(diag, tc.reason) {
			t.Fatalf("%s: %d %q %q", tc.file, code, out, diag)
		}
	}
}

func TestSwarmListCLIEscapesTerminalCellsWithoutChangingJSON(t *testing.T) {
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("listing used %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"}]`)
		case "/api/projects/p1/swarms":
			fmt.Fprint(w, `[{"id":"s\t1","name":"a\nb\u001b[31m","state":"active\rshown"}]`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	code, out, diagnostic := invoke(t, "", "swarm", "list")
	want := "ID\tNAME\tPROJECT\tSTATE\n" + `s\t1` + "\t" + `a\nb\x1b[31m` + "\tp1\t" + `active\rshown` + "\n"
	if code != 0 || out != want || diagnostic != "" {
		t.Fatalf("unsafe table: code=%d out=%q diagnostic=%q", code, out, diagnostic)
	}
	code, out, diagnostic = invoke(t, "", "swarm", "list", "--json")
	var rows []map[string]string
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	wantRows := []map[string]string{{"id": "s\t1", "name": "a\nb\x1b[31m", "projectId": "p1", "state": "active\rshown"}}
	if code != 0 || diagnostic != "" || !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("JSON values changed: code=%d rows=%q diagnostic=%q", code, rows, diagnostic)
	}
}

func TestSwarmListCLI(t *testing.T) {
	var requests []string
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"},{"id":"p2","name":"other"},{"id":"p3","name":"demo"}]`)
		case "/api/projects/p1/swarms":
			fmt.Fprint(w, `[{"id":"s1","name":"main","state":"active"},{"id":"s9","name":"solo","state":"deleted"}]`)
		case "/api/projects/p2/swarms":
			fmt.Fprint(w, `[{"id":"s2","name":"build","state":"active"}]`)
		case "/api/projects/p3/swarms":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	table := "ID\tNAME\tPROJECT\tSTATE\n" +
		"s1\tmain\tp1\tactive\n" +
		"s9\tsolo\tp1\tdeleted\n" +
		"s2\tbuild\tp2\tactive\n"
	code, out, diag := invoke(t, "", "swarm", "list")
	if code != 0 || out != table || diag != "" {
		t.Fatalf("list: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "list", "--json")
	want := `[{"id":"s1","name":"main","projectId":"p1","state":"active"},` +
		`{"id":"s9","name":"solo","projectId":"p1","state":"deleted"},` +
		`{"id":"s2","name":"build","projectId":"p2","state":"active"}]` + "\n"
	if code != 0 || out != want || diag != "" {
		t.Fatalf("list json: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "list", "--project", "p1")
	if code != 0 || out != "ID\tNAME\tPROJECT\tSTATE\ns1\tmain\tp1\tactive\ns9\tsolo\tp1\tdeleted\n" || diag != "" {
		t.Fatalf("list project id: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "list", "--project=other", "--json")
	if code != 0 || out != `[{"id":"s2","name":"build","projectId":"p2","state":"active"}]`+"\n" || diag != "" {
		t.Fatalf("list project name: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "list", "--project", "p3")
	if code != 0 || out != "no swarms\n" || diag != "" {
		t.Fatalf("list empty: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "list", "--project", "p3", "--json")
	if code != 0 || out != "[]\n" || diag != "" {
		t.Fatalf("list empty json: %d %q %q", code, out, diag)
	}
	for _, tc := range []struct {
		project string
		reason  string
	}{
		{"demo", "project name is ambiguous; use its ID"},
		{"ghost", "project not found or not accessible"},
	} {
		code, out, diag = invoke(t, "", "swarm", "list", "--project", tc.project)
		if code != 1 || out != "" || !strings.Contains(diag, tc.reason) {
			t.Fatalf("project %s: %d %q %q", tc.project, code, out, diag)
		}
	}
	for _, request := range requests {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("listing mutated: %s", request)
		}
	}
}

// TestSwarmListCLIRefusals — отказ API не становится пустым или частичным
// выводом: серверная диагностика не печатается, истёкшая сессия сохраняет
// обычный код выхода и подсказку входа.
func TestSwarmListCLIRefusals(t *testing.T) {
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects" {
			fmt.Fprint(w, `[{"id":"p1","name":"demo"},{"id":"p2","name":"other"}]`)
			return
		}
		if r.URL.Path == "/api/projects/p1/swarms" {
			fmt.Fprint(w, `[{"id":"s1","name":"main","state":"active"}]`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "internal secret diagnostics")
	})
	code, out, diag := invoke(t, "", "swarm", "list")
	if code != 1 || out != "" || !strings.Contains(diag, "server error") || strings.Contains(diag, "internal secret diagnostics") {
		t.Fatalf("mid-list refusal: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "list", "--json")
	if code != 1 || out != "" || strings.Contains(diag, "internal secret diagnostics") {
		t.Fatalf("mid-list json refusal: %d %q %q", code, out, diag)
	}
}

func TestSwarmListCLIAuthExpiry(t *testing.T) {
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "dummy-sensitive-value")
	})
	code, out, diag := invoke(t, "", "swarm", "list")
	if code != 2 || out != "" || strings.Contains(diag, "dummy-sensitive-value") || !strings.Contains(diag, "run bro login") {
		t.Fatalf("expiry: %d %q %q", code, out, diag)
	}
}

func TestSwarmCLIRejectsArguments(t *testing.T) {
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid arguments made a request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, args := range [][]string{
		{"swarm"},
		{"swarm", "roles"},
		{"swarm", "roles", "s1", "extra"},
		{"swarm", "roles", "s1", "--definition"},
		{"swarm", "roles", "s1", "--definition="},
		{"swarm", "roles", "s1", "--definition", "a", "--definition", "b"},
		{"swarm", "bind", "s1", "build"},
		{"swarm", "bind", "s1", "build", "master", "extra"},
		{"swarm", "bind", "s1", "build", "master", "--definition", "a"},
		{"swarm", "unbind", "s1"},
		{"swarm", "unbind", "s1", "build", "master"},
		{"swarm", "start", "s1"},
		{"swarm", "start", "s1", "--definition", "release"},
		{"swarm", "start", "s1", "--goal", "Fix #42."},
		{"swarm", "start", "s1", "--definition", "release", "--goal", "a", "--goal-file", "goal.md"},
		{"swarm", "start", "s1", "--definition", "release", "--goal"},
		{"swarm", "start", "s1", "--definition", "release", "--goal="},
		{"swarm", "start", "s1", "--definition", "release", "--goal", "a", "--goal", "b"},
		{"swarm", "start", "s1", "--definition", "release", "--goal-file"},
		{"swarm", "start", "s1", "--definition", "release", "--goal", "a", "--commit"},
		{"swarm", "start", "s1", "extra", "--definition", "release", "--goal", "a"},
		{"swarm", "start", "--definition", "release", "--goal", "a"},
		{"swarm", "stop"},
		{"swarm", "stop", "s1", "extra"},
		{"swarm", "stop", "s1", "--commit", "abc"},
		{"swarm", "stop", "s1", "--definition", "release"},
		{"swarm", "roles", "s1", "--goal", "a"},
		{"swarm", "bind", "s1", "build", "master", "--commit", "abc"},
		{"swarm", "roles", "s1", "--force"},
		{"swarm", "roles", ""},
		{"swarm", "list", "extra"},
		{"swarm", "list", "--project"},
		{"swarm", "list", "--project="},
		{"swarm", "list", "--project", "p1", "--project", "p2"},
		{"swarm", "list", "--definition", "release"},
		{"swarm", "list", "--json", "--json"},
		{"swarm", "list", "--project", "p1", "--json", "extra"},
	} {
		code, out, diag := invoke(t, "", args...)
		if code != 1 || out != "" || diag == "" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
	code, out, diag := invoke(t, "", "--guest", "swarm", "list")
	if code != 1 || !strings.Contains(diag, "guest mode") {
		t.Fatalf("guest swarm list: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "--help")
	if code != 0 || !strings.Contains(out, "bro swarm bind SWARM ROLE ACTOR") || !strings.Contains(out, "bro swarm start SWARM --definition NAME (--goal TEXT | --goal-file FILE) [--commit SHA]") || !strings.Contains(out, "bro swarm stop SWARM") || diag != "" {
		t.Fatalf("help: %d %q %q", code, out, diag)
	}
}
