// FEATURE: Publish standalone workflow snapshots during operator builds.
// SPEC: Publication is atomic with project sync, precedes Docker, and never deletes absent workflows.
// IMPLEMENTATION: Send standalone snapshots with the project declaration.
// RESULT: IMPLEMENTED — builds publish declarations without deleting undeclared workflows.
// STATUS: DONE
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/pipeline"
)

const buildWorkflowFixture = `
workflows:
  check:
    start: work
    exit: done
    nodes:
      work: {type: actor, role: build, prompt: Check the project}
      done: {type: exit}
    edges:
      - {from: work, to: done}
    goalGates: [done]
  review:
    start: inspect
    exit: end
    nodes:
      inspect: {type: actor, role: build, prompt: Review the project}
      end: {type: exit}
    edges:
      - {from: inspect, to: end}
    goalGates: [end]
`

func addBuildWorkflows(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "Broodfile")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(buildWorkflowFixture); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// BUG: Oversized workflow batches mutate the project before rejection.
// HYPOTHESIS: Publication limits are checked only after ProjectSync.
// SOLUTION: Validate the publication request before project synchronization.
// RESULT: FIXED — oversized batches cause no server mutation or Docker build.
// STATUS: FIXED
func TestBuildRejectsTooManyWorkflowsBeforeSync(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	f, err := os.OpenFile(filepath.Join(root, "Broodfile"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "\nworkflows:")
	for i := 0; i < 257; i++ {
		fmt.Fprintf(f, "  flow%d:\n    start: work\n    exit: done\n    nodes:\n      work: {type: actor, role: build, prompt: Check}\n      done: {type: exit}\n    edges: [{from: work, to: done}]\n    goalGates: [done]\n", i)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"project-id","name":"example","origin_mode":"gateway"}`))
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	docker := fakeDocker(t, "exit 42\n")
	code, _, diagnostic := invoke(t, "", "build")
	if code == 0 || requests != 0 {
		t.Fatalf("code=%d requests=%d diagnostic=%s", code, requests, diagnostic)
	}
	if _, err := os.Stat(docker); !os.IsNotExist(err) {
		t.Fatal("Docker invoked")
	}
}

func TestBuildPublishesWorkflowSnapshots(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	addBuildWorkflows(t, root)
	t.Chdir(root)
	var calls []string
	stored := map[string]string{"undeclared": "existing workflow"}
	var published map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/projects/sync":
			if r.Method != http.MethodPut {
				t.Errorf("publication method = %s", r.Method)
			}
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != "fixture-session" || r.Header.Get("X-CSRFToken") != "fixture-csrf" {
				t.Error("publication omitted session or CSRF")
			}
			var body struct {
				Workflows map[string]string `json:"workflows"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			published = body.Workflows
			for name, snapshot := range published {
				stored[name] = snapshot
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"project-id","name":"example","repository":"https://git.example.com/team/current.git","origin_mode":"gateway"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	dockerCalls := fakeDocker(t, "printf 'fixture docker failure\\n' >&2\nexit 42\n")
	_, _, diagnostic := invoke(t, "", "build")
	if calls, err := os.ReadFile(dockerCalls); err != nil || len(calls) == 0 {
		t.Fatalf("Docker was not reached after successful publication: calls=%q error=%v diagnostic=%s", calls, err, diagnostic)
	}
	wantCalls := []string{"PUT /api/projects/sync"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("requests = %v, want %v; diagnostic: %s", calls, wantCalls, diagnostic)
	}
	if len(published) != 2 {
		t.Fatalf("published workflows = %v, want check and review", published)
	}
	for name, want := range map[string]struct{ start, exit, prompt string }{
		"check":  {"work", "done", "Check the project"},
		"review": {"inspect", "end", "Review the project"},
	} {
		graph, err := pipeline.Parse(published[name], name+".yaml")
		if err != nil {
			t.Fatalf("%s is not a standalone pipeline: %v", name, err)
		}
		if graph.Name != name || graph.Start != want.start || graph.Exit != want.exit || graph.Nodes[want.start].Role != "build" || graph.Nodes[want.start].Prompt != want.prompt || len(graph.Edges) != 1 || graph.Edges[0].From != want.start || graph.Edges[0].To != want.exit {
			t.Errorf("%s snapshot = %+v", name, graph)
		}
	}
	if stored["undeclared"] != "existing workflow" {
		t.Fatal("undeclared workflow was removed")
	}
}

func TestBuildWorkflowPublicationFailure(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	addBuildWorkflows(t, root)
	t.Chdir(root)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "atomic sync unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	fakeDocker(t, "exit 42\n")
	code, _, diagnostic := invoke(t, "", "build")
	if code == 0 || !strings.Contains(diagnostic, "503") || !strings.Contains(diagnostic, "project synchronization") {
		t.Fatalf("atomic sync failure must report 503: code=%d diagnostic=%q", code, diagnostic)
	}
}

// BUG: Older servers reject workflows with the same 400 as other invalid declarations.
// HYPOTHESIS: Retrying without workflows would silently lose atomic publication.
// SOLUTION: Report only allowlisted reasons and do not retry; document server-first upgrades.
func TestBuildPreservesAmbiguousSyncErrors(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		safe         bool
	}{
		{"unknown", "another invalid declaration", false},
		{"secret", "token=secret-value", false},
		{"safe reason with secret", "invalid or oversized project declaration: token=secret-value", false},
		{"invalid declaration", "invalid or oversized project declaration", true},
		{"workflow count", "workflow limit exceeded: too many workflow definitions", true},
		{"workflow size", "workflow limit exceeded: workflow definition batch exceeds size limit", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			root := gitProject(t)
			addBuildOrigin(t, root)
			addBuildWorkflows(t, root)
			t.Chdir(root)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"detail":%q}`, tc.detail)
			}))
			defer server.Close()
			saveBuildSession(t, server.URL)
			docker := fakeDocker(t, "exit 42\n")
			code, _, diagnostic := invoke(t, "", "build")
			if code == 0 || !strings.Contains(diagnostic, "400") || !strings.Contains(diagnostic, "project synchronization") {
				t.Fatalf("sync failure not reported: code=%d diagnostic=%q", code, diagnostic)
			}
			if strings.Contains(diagnostic, tc.detail) != tc.safe || strings.Contains(diagnostic, "secret-value") {
				t.Fatalf("unsafe or missing server detail: diagnostic=%q", diagnostic)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want exactly one", requests)
			}
			if _, err := os.Stat(docker); !os.IsNotExist(err) {
				t.Fatal("Docker ran after synchronization failure")
			}
		})
	}
}

func TestBuildWithoutWorkflowsSkipsPublication(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	t.Chdir(root)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"project-id","name":"example","origin_mode":"gateway"}`))
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	fakeDocker(t, "exit 42\n")
	invoke(t, "", "build")
	if !reflect.DeepEqual(calls, []string{"PUT /api/projects/sync"}) {
		t.Fatalf("requests without workflows = %v", calls)
	}
}

func TestBuildWorkflowsOfflineModes(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildWorkflows(t, root)
	t.Chdir(root)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
	defer server.Close()
	saveBuildSession(t, server.URL)
	docker := fakeDocker(t, "exit 42\n")
	for _, args := range [][]string{{"build", "--plan"}, {"build", "--export", filepath.Join(t.TempDir(), "context")}} {
		code, _, diagnostic := invoke(t, "", args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, diagnostic)
		}
	}
	if requests != 0 {
		t.Fatalf("offline modes made %d requests", requests)
	}
	if _, err := os.Stat(docker); !os.IsNotExist(err) {
		t.Fatal("offline modes invoked Docker")
	}
}
