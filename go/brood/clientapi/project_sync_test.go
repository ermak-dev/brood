package clientapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"brood.dev/go/brood/pipeline"
)

// BUG: Клиент зависит от серверного хранилища.
// HYPOTHESIS: Клиент импортирует store для общих ограничений публикации.
// SOLUTION: Общие ограничения перенесены в пакет pipeline.
// RESULT: FIXED — зависимости клиента больше не включают store.
// STATUS: FIXED
func TestClientHasNoStoreDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("list dependencies: %v: %s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "brood.dev/go/brood/store" {
			t.Fatalf("clientapi depends on server store: %s", dep)
		}
	}
}

func TestProjectSyncPublicErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"known", `{"detail":"invalid or oversized project declaration"}`, "invalid or oversized project declaration", 400},
		{"limit", `{"detail":"workflow limit exceeded: too many workflow definitions"}`, "workflow limit exceeded: too many workflow definitions", 400},
		{"secret", `{"detail":"secret-token"}`, "", 400},
		{"suffix", `{"detail":"invalid or oversized project declaration secret-token"}`, "", 400},
		{"other status", `{"detail":"invalid or oversized project declaration","secret":"secret-token"}`, "", 500},
		{"oversized", `{"detail":"secret-token","padding":"` + strings.Repeat("x", 4096) + `"}`, "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			c, err := New(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.ProjectSync(context.Background(), map[string]any{"name": "demo"})
			if err == nil || strings.Contains(err.Error(), "secret-token") || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) || (tc.want == "" && strings.Contains(err.Error(), "invalid or oversized project declaration")) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestWorkflowPublicationLimits(t *testing.T) {
	if pipeline.MaxWorkflowDefinitions != 256 || pipeline.MaxWorkflowNameBytes != 63 || pipeline.MaxWorkflowDefinitionBytes != 1<<20 || pipeline.MaxWorkflowBatchBytes != 4<<20 || pipeline.MaxWorkflowBodyBytes != 24<<20 {
		t.Fatal("workflow publication limits changed")
	}
	name := strings.Repeat("n", 63)
	if _, err := ValidateWorkflowPublication(map[string]string{name: "x"}); err != nil {
		t.Fatalf("name boundary rejected: %v", err)
	}
	if _, err := ValidateWorkflowPublication(map[string]string{name + "n": "x"}); err == nil {
		t.Fatal("excess name size accepted")
	}
	workflows := make(map[string]string)
	for i := 0; i < pipeline.MaxWorkflowDefinitions; i++ {
		workflows[fmt.Sprintf("flow%d", i)] = "small"
	}
	if _, err := ValidateWorkflowPublication(workflows); err != nil {
		t.Fatalf("count boundary rejected: %v", err)
	}
	workflows["extra"] = "small"
	if _, err := ValidateWorkflowPublication(workflows); err == nil {
		t.Fatal("excess workflow count accepted")
	}
	workflows = map[string]string{"a": strings.Repeat("x", pipeline.MaxWorkflowDefinitionBytes)}
	if _, err := ValidateWorkflowPublication(workflows); err != nil {
		t.Fatalf("definition boundary rejected: %v", err)
	}
	workflows["a"] += "x"
	if _, err := ValidateWorkflowPublication(workflows); err == nil {
		t.Fatal("excess definition size accepted")
	}
	workflows = map[string]string{}
	for _, name := range []string{"a", "b", "c", "d"} {
		workflows[name] = strings.Repeat("x", pipeline.MaxWorkflowDefinitionBytes-1)
	}
	if _, err := ValidateWorkflowPublication(workflows); err != nil {
		t.Fatalf("aggregate boundary rejected: %v", err)
	}
	workflows["e"] = "x"
	if _, err := ValidateWorkflowPublication(workflows); err == nil {
		t.Fatal("excess aggregate size accepted")
	}
	delete(workflows, "e")
	for name := range workflows {
		workflows[name] = strings.Repeat("\x00", pipeline.MaxWorkflowDefinitionBytes-1)
	}
	if _, err := ValidateWorkflowPublication(workflows); err == nil || !strings.Contains(err.Error(), "request") {
		t.Fatalf("escaped request over body limit: %v", err)
	}
}

// BUG: Публикация workflow отклоняет успешный ответ 200 с JSON.
// HYPOTHESIS: Вызов с nil вместо результата ошибочно считает ответ 200 ошибкой.
// SOLUTION: Декодировать ответ публикации в json.RawMessage, сохраняя проверку JSON.
// RESULT: FIXED — ответ 200 принимается, ошибки и некорректный JSON отклоняются.
// STATUS: FIXED
func TestPublishWorkflowsResponses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"json", http.StatusOK, `{}`, false},
		{"no content", http.StatusNoContent, ``, false},
		{"client error", http.StatusBadRequest, `bad request`, true},
		{"malformed success", http.StatusOK, `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/api/projects/p/workflows" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			c, err := New(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = c.PublishWorkflows(context.Background(), "p", map[string]string{"check": "definition"})
			if (err != nil) != tc.wantError {
				t.Fatalf("PublishWorkflows() error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

// FEATURE: Типизированный клиент синхронизации проекта #172.
// SPEC: PUT использует существующий CSRF/session transport и точные JSON поля.
// IMPLEMENTATION: Типизированный результат и поддержка 201 в общем transport.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestProjectSyncClient(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-CSRFToken") != "csrf" || r.Header.Get("Origin") == "" {
			t.Error("missing normal CSRF transport")
		}
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "session" {
			t.Error("missing normal session transport")
		}
		if r.Method != "PUT" || r.URL.Path != "/api/projects/sync" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "demo" || body["repository"] != "" {
			t.Errorf("body: %v %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"p","name":"demo","repository":"","created":true,"changed":true,"origin_mode":""}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	resultValue, err := c.ProjectSync(context.Background(), map[string]any{"name": "demo", "repository": ""})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(resultValue)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	json.Unmarshal(data, &result)
	if calls != 1 || result["id"] != "p" || result["name"] != "demo" || result["repository"] != "" || result["created"] != true || result["changed"] != true || result["origin_mode"] != "" {
		t.Fatalf("result: %s, calls=%d", data, calls)
	}
}
