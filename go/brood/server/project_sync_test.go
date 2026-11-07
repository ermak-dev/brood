package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/pipeline"
)

// FEATURE: Explain workflow project limits.
// SPEC: A merged definition limit rejection identifies the exceeded limit.
// IMPLEMENTATION: Return the store limit error to the client.
// RESULT: IMPLEMENTED — the workflow endpoint reports the limit.
// STATUS: DONE
func TestWorkflowLimitResponse(t *testing.T) {
	ts, admin, _, _, id := gitRepositorySetup(t)
	for i := 0; i < pipeline.MaxWorkflowDefinitions; i++ {
		if _, err := ts.srv.catalog.db.Exec(`INSERT INTO workflow_definitions(project_id,name,yaml,version) VALUES (?,?,?,1)`, id, fmt.Sprintf("old%d", i), workflowDocument("test")); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/projects/" + id + "/workflows"
	r, _, raw := ts.do(admin, "PUT", path, ts.csrf(admin), map[string]any{"workflows": map[string]string{"test": workflowDocument("test")}})
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(raw, "too many workflow definitions") {
		t.Fatalf("workflow limit: status=%d body=%s", r.StatusCode, raw)
	}
}

// FEATURE: Декларативная синхронизация проекта #172.
// SPEC: Атомарное создание/изменение администратором, noop для участника,
// строгий JSON, сохранение отсутствующих свойств и отсутствие побочных эффектов.
// BUG: Project and workflow publication can commit different declarations.
// HYPOTHESIS: They are sent and committed in separate requests.
func TestProjectSyncWorkflowAtomicity(t *testing.T) {
	ts, admin, _, _, _ := gitRepositorySetup(t)
	workflow := "apiVersion: brood.dev/v1alpha1\nname: demo\nstart: s\nexit: e\nnodes:\n  s: {type: actor, prompt: go}\n  l: {type: land, target: main}\n  e: {type: exit}\nedges:\n  - {from: s, to: l}\n  - {from: l, to: e}\ngoalGates: [l]\n"
	declaration := map[string]any{"name": "atomic", "repository": "", "description": "before", "workflows": map[string]string{"demo": workflow}}
	r, b, _ := ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "initial sync", r, b, 201)
	id := b["id"].(string)
	_, err := ts.srv.catalog.db.Exec(`CREATE TRIGGER reject_workflow_update BEFORE UPDATE ON workflow_definitions BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	declaration["description"] = "after"
	declaration["workflows"] = map[string]string{"demo": workflow + "\n"}
	r, _, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	if r.StatusCode < 400 {
		t.Fatalf("expected workflow failure, got %d", r.StatusCode)
	}
	var description string
	if err := ts.srv.catalog.db.QueryRow(`SELECT description FROM command_project_details WHERE project_id=?`, id).Scan(&description); err != nil || description != "before" {
		t.Fatalf("partial project commit: description=%q err=%v", description, err)
	}
}

// BUG: Concurrent syncs can leave a project paired with another request's workflow.
// HYPOTHESIS: Separate project and workflow commits can interleave.
func TestProjectSyncWorkflowConcurrent(t *testing.T) {
	ts, admin, _, _, _ := gitRepositorySetup(t)
	csrf := ts.csrf(admin)
	workflow := "apiVersion: brood.dev/v1alpha1\nname: demo\nstart: s\nexit: e\nnodes:\n  s: {type: actor, prompt: VALUE}\n  l: {type: land, target: main}\n  e: {type: exit}\nedges:\n  - {from: s, to: l}\n  - {from: l, to: e}\ngoalGates: [l]\n"
	request := func(value string) map[string]any {
		return map[string]any{"name": "concurrent-workflow", "repository": "", "description": value, "workflows": map[string]string{"demo": strings.ReplaceAll(workflow, "VALUE", value)}}
	}
	r, b, _ := ts.do(admin, "PUT", "/api/projects/sync", csrf, request("initial"))
	expectStatus(t, "initial", r, b, 201)
	id := b["id"].(string)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, value := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, _ := ts.do(admin, "PUT", "/api/projects/sync", csrf, request(value))
			codes <- r.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("sync returned %d", code)
		}
	}
	var description, text string
	if err := ts.srv.catalog.db.QueryRow(`SELECT description FROM command_project_details WHERE project_id=?`, id).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.catalog.db.QueryRow(`SELECT yaml FROM workflow_definitions WHERE project_id=? AND name='demo'`, id).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != strings.ReplaceAll(workflow, "VALUE", description) {
		t.Fatalf("mismatched project %q and workflow %q", description, text)
	}
}

func TestProjectSyncLifecycle(t *testing.T) {
	ts, admin, alice, carol, existing := gitRepositorySetup(t)
	ctx := context.Background()
	var dials atomic.Int64
	ts.srv.git.transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("declaration must not fetch")
	}
	t.Cleanup(func() {
		if dials.Load() != 0 {
			t.Error("project declaration attempted a Git dial")
		}
	})
	beforeCapacity, _ := ts.srv.store.GetProjectCapacity(ctx, existing)
	declaration := map[string]any{"name": "declarative", "repository": "https://private.invalid/team/repo.git", "description": "initial"}
	r, b, _ := ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "sync create", r, b, 201)
	id, ok := b["id"].(string)
	if !ok || id == "" {
		t.Fatalf("missing id: %v", b)
	}
	if b["created"] != true || b["changed"] != true || b["name"] != "declarative" || b["repository"] != declaration["repository"] || b["origin_mode"] != "" {
		t.Fatalf("create result: %v", b)
	}
	var description string
	if err := ts.srv.catalog.db.QueryRowContext(ctx, `SELECT description FROM command_project_details WHERE project_id=?`, id).Scan(&description); err != nil || description != "initial" {
		t.Fatalf("initial description: %q %v", description, err)
	}
	cap, _ := ts.srv.store.GetProjectCapacity(ctx, id)
	if !reflect.DeepEqual(cap, beforeCapacity) {
		t.Fatalf("default capacity differs: %v / %v", cap, beforeCapacity)
	}
	members, _ := ts.srv.store.MembersOf(ctx, id)
	swarms, _ := ts.srv.gateway.SwarmsByProject(id)
	if len(members) != 0 || len(swarms) != 0 {
		t.Fatal("sync created membership or swarm")
	}
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "sync repeat", r, b, 200)
	if b["id"] != id || b["changed"] != false || b["created"] != false {
		t.Fatalf("repeat: %v", b)
	}
	for _, client := range []*http.Client{alice, carol} {
		r, _, _ = ts.do(client, "PUT", "/api/projects/sync", ts.csrf(client), declaration)
		if r.StatusCode != 403 {
			t.Fatalf("outsider noop: %d", r.StatusCode)
		}
	}
	r, b, _ = ts.do(admin, "POST", "/api/projects/"+id+"/members", ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "add member", r, b, 204)
	r, b, _ = ts.do(alice, "PUT", "/api/projects/sync", ts.csrf(alice), declaration)
	expectStatus(t, "member noop", r, b, 200)
	declaration["repository"] = "https://github.com/org/new"
	r, _, _ = ts.do(alice, "PUT", "/api/projects/sync", ts.csrf(alice), declaration)
	if r.StatusCode != 403 {
		t.Fatalf("member change: %d", r.StatusCode)
	}
	declaration["description"] = ""
	declaration["capacity"] = map[string]any{"worker_selector": map[string]string{"pool": "special"}, "max_actors": "7", "max_swarms": nil, "max_cpu_millicores": "1000", "max_memory_bytes": "2048"}
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "update", r, b, 200)
	if b["changed"] != true || b["created"] != false || b["origin_mode"] != "direct" {
		t.Fatalf("update result: %v", b)
	}
	cap, err := ts.srv.store.GetProjectCapacity(ctx, id)
	if err != nil || cap.MaxActors == nil || *cap.MaxActors != 7 || cap.WorkerSelector["pool"] != "special" {
		t.Fatalf("capacity: %v %v", cap, err)
	}
	if err := ts.srv.catalog.db.QueryRowContext(ctx, `SELECT description FROM command_project_details WHERE project_id=?`, id).Scan(&description); err != nil || description != "" {
		t.Fatalf("cleared description: %q %v", description, err)
	}
	delete(declaration, "capacity")
	delete(declaration, "description")
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "absent preserves", r, b, 200)
	if b["changed"] != false {
		t.Fatalf("absent changed: %v", b)
	}
	after, _ := ts.srv.store.GetProjectCapacity(ctx, id)
	if !reflect.DeepEqual(cap, after) {
		t.Fatal("absent capacity overwritten")
	}
}

func syncRaw(t *testing.T, ts *testServer, client *http.Client, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", ts.http.URL+"/api/projects/sync", strings.NewReader(body))
	req.Header.Set("Origin", ts.http.URL)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderCSRF, ts.csrf(client))
	for _, cookie := range client.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	return w
}

func TestProjectSyncStrictRequests(t *testing.T) {
	ts, admin, _, _, _ := gitRepositorySetup(t)
	for _, body := range []string{
		``, `null`, `[]`, `{}`, `{"name":"x"}`, `{"repository":""}`, `{"name":null,"repository":""}`, `{"name":"","repository":""}`,
		`{"name":"x","repository":null}`, `{"name":"x","repository":"","name":"x"}`, `{"Name":"x","repository":""}`,
		`{"name":"x","repository":"","secret":"synthetic-private"}`, `{"name":"x","repository":"","description":null}`,
		`{"name":"x","repository":"","capacity":null}`, `{"name":"x","repository":"","capacity":{"max_actors":7}}`,
		`{"name":"x","repository":"","capacity":{"max_actors":"01"}}`, `{"name":"x","repository":"","capacity":{"max_actors":"9223372036854775808"}}`,
		`{"name":"x","repository":"","capacity":{"max_actors":"1","max_actors":"2"}}`,
		`{"name":"x","repository":"","capacity":{"worker_selector":{"a":"b","a":"c"}}}`,
		`{"name":"x","repository":"","capacity":{"worker_selector":{"a":null}}}`,
		`{"name":"x","repository":"","capacity":{"extra":1}}`,
		`{"name":"x","repository":""} {}`, `{"name":"x","repository":"https://user:synthetic-private@private.invalid/a"}`,
		`{"name":"x","repository":"http://private.invalid/a"}`, `{"name":"x","repository":"","description":"` + strings.Repeat("x", 70000) + `"}`,
	} {
		w := syncRaw(t, ts, admin, body)
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "synthetic-private") {
			t.Fatalf("invalid body status=%d: %s", w.Code, w.Body.String())
		}
	}
}

func TestProjectSyncConcurrentRepeats(t *testing.T) {
	ts, admin, _, _, _ := gitRepositorySetup(t)
	csrf := ts.csrf(admin)
	var wg sync.WaitGroup
	results := make(chan map[string]any, 8)
	codes := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, b, _ := ts.do(admin, "PUT", "/api/projects/sync", csrf, map[string]string{"name": "concurrent", "repository": ""})
			results <- b
			codes <- r.StatusCode
		}()
	}
	wg.Wait()
	close(results)
	close(codes)
	creates := 0
	for code := range codes {
		if code == 201 {
			creates++
		} else if code != 200 {
			t.Fatalf("concurrent status: %d", code)
		}
	}
	if creates != 1 {
		t.Fatalf("created %d times", creates)
	}
	id := ""
	for b := range results {
		if id == "" {
			id, _ = b["id"].(string)
		}
		if b["id"] != id {
			t.Fatalf("duplicate projects: %v", b)
		}
	}
}

func TestProjectSyncRechecksAuthority(t *testing.T) {
	for _, change := range []string{"revoked", "disabled", "membership"} {
		t.Run(change, func(t *testing.T) {
			ts, admin, alice, _, id := gitRepositorySetup(t)
			ctx := context.Background()
			user, _ := ts.srv.store.GetUserByUsername(ctx, "alice")
			body := `{"name":"repo-project","repository":""}`
			reader := &projectSecretAuthorityReader{Reader: strings.NewReader(body), beforeRead: func() {
				var err error
				switch change {
				case "revoked":
					_, err = auth.NewSessions(ts.srv.store, 0).RevokeAllOfUser(ctx, user.ID)
				case "disabled":
					err = ts.srv.store.SetUserDisabled(ctx, user.ID, true)
				case "membership":
					err = ts.srv.store.RemoveMember(ctx, id, user.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			req := httptest.NewRequest("PUT", ts.http.URL+"/api/projects/sync", reader)
			req.Header.Set("Origin", ts.http.URL)
			req.Header.Set(auth.HeaderCSRF, ts.csrf(alice))
			for _, c := range alice.Jar.Cookies(req.URL) {
				req.AddCookie(c)
			}
			w := httptest.NewRecorder()
			ts.srv.Handler().ServeHTTP(w, req)
			if w.Code != 403 {
				t.Fatalf("late %s: %d %s", change, w.Code, w.Body.String())
			}
			_ = admin
		})
	}
}

func TestProjectRepositoryAcceptsUnconfigured(t *testing.T) {
	ts, admin, _, _, id := gitRepositorySetup(t)
	r, b, _ := ts.do(admin, "PUT", "/api/projects/"+id+"/repository", ts.csrf(admin), map[string]string{"origin": "https://private.invalid/repo"})
	expectStatus(t, "unconfigured declaration", r, b, 200)
}

func TestProjectSyncNoopDoesNotWrite(t *testing.T) {
	ts, admin, alice, _, id := gitRepositorySetup(t)
	ctx := context.Background()
	if err := ts.srv.store.PutProjectSecretVars(ctx, id, map[string]string{"TOKEN": "synthetic-private"}); err != nil {
		t.Fatal(err)
	}
	before, err := ts.srv.store.ReadProjectSecretBundle(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"projects", "project_repositories", "command_project_details", "project_capacity", "project_members", "swarms", "project_secret_vars", "project_secret_files", "project_secret_revisions"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			_, err := ts.srv.catalog.db.ExecContext(ctx, `CREATE TRIGGER sync_guard_`+table+event+` BEFORE `+event+` ON `+table+` BEGIN SELECT RAISE(ABORT, 'unexpected row write'); END`)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, client := range []*http.Client{admin, alice} {
		r, b, _ := ts.do(client, "PUT", "/api/projects/sync", ts.csrf(client), map[string]any{"name": "repo-project", "repository": "", "capacity": map[string]any{"worker_selector": map[string]string{}}})
		expectStatus(t, "write-free noop", r, b, 200)
		if b["changed"] != false {
			t.Fatalf("noop: %v", b)
		}
	}
	after, err := ts.srv.store.ReadProjectSecretBundle(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("secret mutation: %v", err)
	}
}
