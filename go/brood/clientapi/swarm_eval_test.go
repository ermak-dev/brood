package clientapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FEATURE: Typed eval client with explicit selection and bodyless start.
// SPEC: Exact escaped scope, no retries, no requests for incomplete selection.
// IMPLEMENTATION: Типизированные DTO и штатный session transport без start body.
// RESULT: IMPLEMENTED — loopback HTTP проверяет scope, body и CSRF.
// STATUS: DONE
// BUG: Реальный replay API возвращает boolean interruptedChildren, не count.
// HYPOTHESIS: Числовой DTO и синтетическая fixture скрывают отказ decode при
// первом retained replay; проверяется точная JSON-проекция настоящего API.
func TestSwarmEvalClientDecodesBooleanInterruption(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			reply := fmt.Sprintf(`{"swarmId":"candidate","referenceSwarmId":"reference","referencePass":1,"definition":{"name":"flow","version":1,"revision":"revision"},"model":"model","source":{"origin":"https://example.test/repo","commit":"commit"},"goal":"Pinned goal","nonProduction":true,"execution":"stopped","replay":{"requestId":"question","state":"stopped","reason":"Unmatched question","interruptedChildren":%t}}`, interrupted)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, reply) }))
			defer server.Close()
			client, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			status, err := client.SwarmEval(context.Background(), "candidate")
			if err != nil {
				t.Fatalf("real replay field type cannot be decoded: %v", err)
			}
			data, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			var view struct {
				Replay map[string]any `json:"replay"`
			}
			if err := json.Unmarshal(data, &view); err != nil || view.Replay["interruptedChildren"] != interrupted {
				t.Fatalf("replay boolean was changed into a count: %s %v", data, err)
			}
		})
	}
}

func TestSwarmEvalClientReturnsOnlySafeRefusalDetails(t *testing.T) {
	for _, tc := range []struct {
		detail  string
		visible bool
	}{
		{"swarm eval conflict: eval permits only its first pass", true},
		{"swarm eval conflict: bound candidate requires eval-only execution", true},
		{"swarm management denied", true},
		{"server token=must-not-print", false},
		{"swarm eval conflict: server token=must-not-print", false},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"detail": tc.detail})
			}))
			defer server.Close()
			client, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client.Restore(StoredSession{Session: "session", CSRF: "csrf"})
			_, err = client.StartSwarmEval(context.Background(), "candidate")
			if err == nil || strings.Contains(err.Error(), tc.detail) != tc.visible || strings.Contains(err.Error(), "must-not-print") {
				t.Fatalf("eval refusal omitted a safe reason or leaked arbitrary diagnostics: %v", err)
			}
		})
	}
}

func TestSwarmEvalClientRequests(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		path := "/api/swarms/candidate%2Fid/eval"
		method := http.MethodGet
		if calls == 1 {
			method = http.MethodPost
			if string(body) != `{"referenceSwarmId":"ref","referencePass":7,"definition":"flow","model":"model"}` {
				t.Errorf("body: %s", body)
			}
		} else if len(body) != 0 {
			t.Errorf("unexpected body %s", body)
		}
		if calls == 3 {
			path += "/start"
			method = http.MethodPost
		}
		if r.URL.EscapedPath() != path || r.Method != method || r.URL.RawQuery != "" {
			t.Errorf("request: %s %s", r.Method, r.URL)
		}
		if method == http.MethodPost && r.Header.Get("X-CSRFToken") != "csrf" {
			t.Errorf("missing CSRF: %v", r.Header)
		}
		if calls == 3 {
			fmt.Fprint(w, `{"swarmId":"candidate/id","execution":"running","step":null,"definition":null,"source":null}`)
			return
		}
		fmt.Fprint(w, `{"swarmId":"candidate/id","referenceSwarmId":"ref","referencePass":7,"definition":{"name":"flow","version":2,"revision":"rev"},"model":"model","source":{"origin":"https://example.test/repo","commit":"sha"},"goal":"goal","nonProduction":true,"execution":"idle","replay":null}`)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	got, err := c.BindSwarmEval(context.Background(), "candidate/id", SwarmEvalSelection{ReferenceSwarmID: "ref", ReferencePass: 7, Definition: "flow", Model: "model"})
	if err != nil || got.Model != "model" || !got.NonProduction || got.Replay != nil || got.Source.Commit != "sha" {
		t.Fatalf("binding: %#v %v", got, err)
	}
	got, err = c.SwarmEval(context.Background(), "candidate/id")
	if err != nil || got.Goal != "goal" {
		t.Fatalf("status: %#v %v", got, err)
	}
	execution, err := c.StartSwarmEval(context.Background(), "candidate/id")
	if err != nil || execution.Execution != "running" || calls != 3 {
		t.Fatalf("start: %#v %v calls %d", execution, err, calls)
	}
}

func TestSwarmEvalClientInvalidSelectionNoNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	c, _ := New(server.URL)
	for _, s := range []SwarmEvalSelection{{}, {ReferenceSwarmID: "ref", ReferencePass: 0, Definition: "flow", Model: "model"}, {ReferenceSwarmID: "ref", ReferencePass: 1, Definition: "flow"}, {ReferenceSwarmID: "ref", ReferencePass: 1, Model: "model"}} {
		if _, err := c.BindSwarmEval(context.Background(), "candidate", s); err == nil {
			t.Fatal("accepted incomplete selection")
		}
	}
	for _, id := range []string{"", strings.Repeat("s", 257)} {
		if _, err := c.StartSwarmEval(context.Background(), id); err == nil {
			t.Fatal("accepted invalid start")
		}
		if _, err := c.SwarmEval(context.Background(), id); err == nil {
			t.Fatal("accepted invalid status")
		}
	}
	if calls != 0 {
		t.Fatalf("invalid selection reached network: %d", calls)
	}
}
