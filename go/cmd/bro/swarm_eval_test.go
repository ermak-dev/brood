package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Двухэтапный isolated eval через настоящий CLI dispatcher (#293).
// SPEC: Явный bind без boot, отдельный start без body/override/retry, retained status.
// IMPLEMENTATION: Отдельный модуль команд использует обычный session client и resolver.
// RESULT: IMPLEMENTED — реальные HTTP-запросы через production dispatcher проверены.
// STATUS: DONE
const cliEvalStatus = `{"swarmId":"s2","referenceSwarmId":"s1","referencePass":7,"definition":{"name":"candidate-flow","version":3,"revision":"rev3"},"model":"explicit-model","source":{"origin":"https://example.test/repo.git","commit":"pinned-sha"},"goal":"Retained goal\nnot an answer","nonProduction":true,"execution":"stopped","replay":{"requestId":"q1","state":"stopped","reason":"historical answer mismatch\u001b[31m","interruptedChildren":true}}`

func evalDiscovery(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api/projects":
		fmt.Fprint(w, `[{"id":"prod"},{"id":"eval"}]`)
	case "/api/projects/prod/swarms":
		fmt.Fprint(w, `[{"id":"s1","name":"reference","state":"active"}]`)
	case "/api/projects/eval/swarms":
		fmt.Fprint(w, `[{"id":"s2","name":"candidate","state":"active"}]`)
	default:
		return false
	}
	if r.Method != "GET" {
		panic("discovery mutated data")
	}
	return true
}

func TestSwarmEvalCLITwoStagesAndRetainedStatus(t *testing.T) {
	var operations []string
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if evalDiscovery(w, r) {
			return
		}
		operations = append(operations, r.Method+" "+r.URL.Path)
		if r.URL.RawQuery != "" {
			t.Error("unexpected query")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/swarms/s2/eval":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := map[string]any{"referenceSwarmId": "s1", "referencePass": float64(7), "definition": "candidate-flow", "model": "explicit-model"}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("bind selection: %#v", body)
			}
			fmt.Fprint(w, cliEvalStatus)
		case "GET /api/swarms/s2/eval":
			fmt.Fprint(w, cliEvalStatus)
		case "POST /api/swarms/s2/eval/start":
			body, _ := io.ReadAll(r.Body)
			if len(body) != 0 {
				t.Errorf("start body: %s", body)
			}
			fmt.Fprint(w, `{"swarmId":"s2","execution":"running","definition":{"name":"candidate-flow","version":3,"revision":"rev3"},"source":{"origin":"https://example.test/repo.git","commit":"pinned-sha"},"goal":"Retained goal\nnot an answer","step":null}`)
		default:
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
	})
	code, out, diag := invoke(t, "", "swarm", "eval", "bind", "candidate", "--reference", "reference", "--pass", "7", "--definition", "candidate-flow", "--model", "explicit-model")
	if code != 0 || diag != "" {
		t.Fatalf("bind: %d %q %q", code, out, diag)
	}
	for _, part := range []string{"non-production", `reference: "s1" pass 7`, `model: "explicit-model"`, `commit "pinned-sha"`, `goal: "Retained goal\nnot an answer"`, `historical answer mismatch\x1b[31m`, "interrupted children: true"} {
		if !strings.Contains(out, part) {
			t.Errorf("missing %q: %s", part, out)
		}
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Error("terminal injection")
	}
	if len(operations) != 1 {
		t.Fatalf("bind booted/started: %v", operations)
	}
	code, out, diag = invoke(t, "", "swarm", "eval", "start", "candidate", "--json")
	if code != 0 || diag != "" || !strings.Contains(out, `"execution": "running"`) {
		t.Fatalf("start: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "eval", "status", "candidate", "--json")
	var got, want any
	if code != 0 || diag != "" || json.Unmarshal([]byte(out), &got) != nil || json.Unmarshal([]byte(cliEvalStatus), &want) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("retained status: %d %q %q", code, out, diag)
	}
	if !reflect.DeepEqual(operations, []string{"POST /api/swarms/s2/eval", "POST /api/swarms/s2/eval/start", "GET /api/swarms/s2/eval"}) {
		t.Fatalf("operations: %v", operations)
	}
}

func TestSwarmEvalCLIStartTextShowsPinnedExecution(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if evalDiscovery(w, r) {
			return
		}
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/swarms/s2/eval/start" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `{"swarmId":"s2","execution":"running","definition":{"name":"flow","version":3,"revision":"rev3"},"source":{"origin":"https://example.test/repo","commit":"pinned-sha"},"goal":"pinned goal","step":{"sequence":1,"nodeId":"build","state":"pending","detail":"retained detail"}}`)
	})
	code, out, diag := invoke(t, "", "swarm", "eval", "start", "candidate")
	if code != 0 || diag != "" || calls != 1 {
		t.Fatalf("start: %d %q %q calls %d", code, out, diag, calls)
	}
	for _, part := range []string{"non-production", `definition: "flow" version 3 revision "rev3"`, `commit "pinned-sha"`, `goal: "pinned goal"`, `step 1 "build": "pending"`, `retained detail`} {
		if !strings.Contains(out, part) {
			t.Errorf("missing %q: %s", part, out)
		}
	}
}

func TestSwarmOrdinaryStartEvalRefusalDoesNotFallback(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if evalDiscovery(w, r) {
			return
		}
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/swarms/s2/start" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		w.WriteHeader(409)
		fmt.Fprint(w, `{"detail":"swarm eval conflict: bound candidate requires eval-only execution"}`)
	})
	code, out, diag := invoke(t, "", "swarm", "start", "candidate", "--definition", "flow", "--goal", "goal")
	if code != 1 || out != "" || !strings.Contains(diag, "status 409") || calls != 1 {
		t.Fatalf("ordinary start bypassed eval binding: %d %q %q calls %d", code, out, diag, calls)
	}
}

func TestSwarmEvalCLIInvalidSelectionNoNetwork(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	valid := []string{"swarm", "eval", "bind", "candidate", "--reference", "reference", "--pass", "7", "--definition", "flow", "--model", "model"}
	cases := [][]string{{"swarm", "eval"}, {"swarm", "eval", "start"}, {"swarm", "eval", "status", ""}, {"swarm", "eval", "start", "candidate", "--goal", "override"}, {"swarm", "eval", "start", "candidate", "--commit", "sha"}, {"swarm", "eval", "start", "candidate", "--model", "model"}, {"swarm", "eval", "status", "candidate", "--json", "--json"}, {"--guest", "swarm", "eval", "start", "candidate"}}
	for _, index := range []int{5, 7, 9, 11} {
		a := append([]string{}, valid[:index-1]...)
		a = append(a, valid[index+1:]...)
		cases = append(cases, a)
	}
	for _, pass := range []string{"0", "-1", "01", "1.5", "9223372036854775808", ""} {
		a := append([]string{}, valid...)
		a[7] = pass
		cases = append(cases, a)
	}
	cases = append(cases, append(append([]string{}, valid...), "--model", "other"))
	for _, a := range cases {
		code, out, _ := invoke(t, "", a...)
		if code == 0 || out != "" {
			t.Errorf("accepted %v: %d %q", a, code, out)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid selection reached network %d times", calls)
	}
}

func TestSwarmEvalCLIServerRefusalsNeverRetry(t *testing.T) {
	for _, status := range []int{403, 409} {
		for _, action := range []string{"bind", "start", "status"} {
			t.Run(fmt.Sprintf("%d/%s", status, action), func(t *testing.T) {
				calls := 0
				secretServer(t, func(w http.ResponseWriter, r *http.Request) {
					if evalDiscovery(w, r) {
						return
					}
					calls++
					w.WriteHeader(status)
					fmt.Fprint(w, `{"detail":"private-synthetic-error"}`)
				})
				args := []string{"swarm", "eval", action, "candidate"}
				if action == "bind" {
					args = append(args, "--reference", "reference", "--pass", "7", "--definition", "flow", "--model", "model")
				}
				code, out, diag := invoke(t, "", args...)
				if code != 1 || out != "" || !strings.Contains(diag, fmt.Sprintf("status %d", status)) || strings.Contains(diag, "private-synthetic-error") || calls != 1 {
					t.Fatalf("refusal: %d %q %q calls %d", code, out, diag, calls)
				}
			})
		}
	}
}
