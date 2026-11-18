package clientapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const swarmExecutionBody = `{"swarmId":"s1","execution":"running","definition":{"name":"release","version":3,"revision":"rev"},"source":{"origin":"https://git.example.org/team/repo.git","commit":"0123456789abcdef0123456789abcdef01234567"},"goal":"Fix #42.","step":{"sequence":4,"nodeId":"build","state":"pending"}}`

// FEATURE: Client transport for swarm start and stop (#242).
// SPEC: POST /api/swarms/{id}/start sends exactly definition, goal and, when
// named, commit over the normal session and CSRF transport; stop sends no
// body; both read the execution view. Refusal reasons reach the caller only
// when they are one of the public start reasons: a short single line that
// begins with a fixed reason of the start contract.
// IMPLEMENTATION: Typed execution view, explicit wire body, prefix allow list.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSwarmExecutionTransport(t *testing.T) {
	var requests []string
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.EscapedPath())
		if r.Header.Get("X-CSRFToken") != "csrf" || r.Header.Get("Origin") == "" {
			t.Error("missing normal CSRF transport")
		}
		var body map[string]any
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		bodies = append(bodies, body)
		fmt.Fprint(w, swarmExecutionBody)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	ctx := context.Background()
	want := SwarmExecution{SwarmID: "s1", Execution: "running", Definition: &SwarmDefinition{Name: "release", Version: 3, Revision: "rev"}, Source: &SwarmSource{Origin: "https://git.example.org/team/repo.git", Commit: "0123456789abcdef0123456789abcdef01234567"}, Goal: "Fix #42.", Step: &SwarmStep{Sequence: 4, NodeID: "build", State: "pending"}}
	goal := "Fix #42.\n\t<keep> the API & docs"
	for name, call := range map[string]func() (SwarmExecution, error){
		"start": func() (SwarmExecution, error) {
			return c.StartSwarm(ctx, "s/1", SwarmStart{Definition: "release", Goal: goal})
		},
		"start with commit": func() (SwarmExecution, error) {
			return c.StartSwarm(ctx, "s1", SwarmStart{Definition: "release", Goal: goal, Commit: "abc"})
		},
		"stop": func() (SwarmExecution, error) { return c.StopSwarm(ctx, "s1") },
	} {
		got, err := call()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %+v, %v", name, got, err)
		}
	}
	wantBodies := map[string]map[string]any{
		"POST /api/swarms/s%2F1/start": {"definition": "release", "goal": goal},
		"POST /api/swarms/s1/start":    {"definition": "release", "goal": goal, "commit": "abc"},
		"POST /api/swarms/s1/stop":     nil,
	}
	if len(requests) != len(wantBodies) {
		t.Fatalf("requests: %v", requests)
	}
	for i, request := range requests {
		body, ok := wantBodies[request]
		if !ok || !reflect.DeepEqual(bodies[i], body) {
			t.Errorf("%s with body %v", request, bodies[i])
		}
	}
}

// TestSwarmExecutionRefusalReasons — причина из публичного набора запуска
// доходит до вызывающего вместе со своей подробностью; любой другой текст,
// многострочный, с управляющими символами или слишком длинный — нет.
func TestSwarmExecutionRefusalReasons(t *testing.T) {
	detail := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"detail": detail})
	}))
	defer server.Close()
	c, _ := New(server.URL)
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	ctx := context.Background()
	for _, tc := range []struct {
		detail string
		shown  bool
	}{
		{"swarm management denied", true},
		{"swarm not found", true},
		{"outstanding child obligations require explicit reconciliation before automatic progression", true},
		{"outstanding child obligations require explicit reconciliation before automatic progression: synthetic-secret-sentinel", false},
		{"outstanding child obligations require explicit reconciliation before automatic progression\nsynthetic-secret-sentinel", false},
		{"invalid swarm start: expected {definition, goal, commit?} with a definition name and a goal", true},
		{"invalid swarm goal: the goal is empty", true},
		{"swarm state conflict: execution is running; start requires idle, succeeded, failed or stopped", true},
		{`swarm graph is pinned: the swarm runs workflow "release" version 1 revision r; start "other" in another swarm`, true},
		{"swarm graph is pinned: the swarm actors work in repository https://git.example.org/a.git, the project repository is now https://git.example.org/b.git; start the workflow in another swarm", true},
		{`workflow definition is not in the project catalog: "absent"`, true},
		{`workflow graph is not executable: workflow "retrying": retries are not supported`, true},
		{"swarm source is invalid: the project has no repository to pin the source to", true},
		{"swarm source changed during start: the project repository changed while its branch was resolved; start again", true},
		{`role "build": node role is not bound to an actor of the swarm`, true},
		{"cannot resolve the project branch: Git upstream request failed", true},
		{"synthetic-secret-sentinel", false},
		{"swarm not found\x1b[2J", false},
		{"swarm state conflict: execution is running\nsynthetic-secret-sentinel", false},
		{"swarm state conflict: " + strings.Repeat("x", 1024), false},
		{"swarm state conflict", false},
	} {
		detail = tc.detail
		for name, call := range map[string]func() error{
			"start": func() error {
				_, err := c.StartSwarm(ctx, "s1", SwarmStart{Definition: "release", Goal: "g"})
				return err
			},
			"stop": func() error { _, err := c.StopSwarm(ctx, "s1"); return err },
		} {
			err := call()
			if err == nil || strings.HasSuffix(err.Error(), ": "+tc.detail) != tc.shown || strings.Contains(err.Error(), "sentinel") || strings.Contains(err.Error(), "\x1b") {
				t.Fatalf("%s detail %q: %v", name, tc.detail, err)
			}
		}
	}
}

// TestSwarmExecutionInputChecked — пустые обязательные значения отвергаются
// до запроса; пределы текста задачи проверяет сервер.
func TestSwarmExecutionInputChecked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid input made a request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"start without swarm": func() error {
			_, err := c.StartSwarm(ctx, "", SwarmStart{Definition: "release", Goal: "g"})
			return err
		},
		"start without definition": func() error { _, err := c.StartSwarm(ctx, "s1", SwarmStart{Goal: "g"}); return err },
		"start without goal":       func() error { _, err := c.StartSwarm(ctx, "s1", SwarmStart{Definition: "release"}); return err },
		"stop without swarm":       func() error { _, err := c.StopSwarm(ctx, ""); return err },
	} {
		if call() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
