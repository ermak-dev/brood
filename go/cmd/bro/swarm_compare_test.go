package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const cliPassComparison = `{"version":1,"alignment":"visit-position","reference":{"swarmId":"s1","pass":1},"candidate":{"swarmId":"s2","pass":10},"sameGoal":true,"sameSource":true,"sameDefinition":false,"stepCount":{"reference":1,"candidate":1,"delta":0},"childTaskCount":{"reference":0,"candidate":0,"delta":0},"steps":[{"position":1,"node":"same","outcome":"unknown","report":"unknown","reportedOutcome":"unknown","reportedReport":"unknown","reference":{"sequence":1,"nodeId":"build","state":"inflight","reportedOutcome":"success","reportedReport":"Reported\nnot actual\u001b[31m","childTasks":0},"candidate":{"sequence":10,"nodeId":"build","state":"completed","outcome":"fail","report":"Actual retained report","childTasks":0}}]}`

// Сравнение через bro (#293) выбирает оба доступных роя по ID/имени, читает
// только заданную пару, не трактует unknown как success и экранирует terminal text.
func TestSwarmCompareCLIResolvesBothSwarmsAndPreservesUnknownResults(t *testing.T) {
	comparisons := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("comparison mutated data: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p","name":"demo"}]`)
		case "/api/projects/p/swarms":
			fmt.Fprint(w, `[{"id":"s1","name":"reference","state":"active"},{"id":"s2","name":"candidate","state":"active"}]`)
		case "/api/swarms/s1/passes/1/compare":
			comparisons++
			if r.URL.Query().Get("swarm") != "s2" || r.URL.Query().Get("pass") != "10" || len(r.URL.Query()) != 2 {
				t.Errorf("wrong pair: %s", r.URL)
			}
			fmt.Fprint(w, cliPassComparison)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	code, out, diagnostic := invoke(t, "", "swarm", "compare", "reference", "1", "candidate", "10")
	if code != 0 || diagnostic != "" {
		t.Fatalf("comparison failed: %d %q %q", code, out, diagnostic)
	}
	for _, part := range []string{`reference: "s1" pass 1`, `candidate: "s2" pass 10`, "visit-position", "same definition: no", "actual outcome: unknown", `reported outcome: "success"`, `actual outcome: "fail"`, `Reported\nnot actual\x1b[31m`, "elapsed milliseconds: unknown"} {
		if !strings.Contains(out, part) {
			t.Fatalf("comparison omitted %q: %s", part, out)
		}
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Fatal("actor report injected a terminal escape")
	}
	code, out, diagnostic = invoke(t, "", "swarm", "compare", "s1", "1", "s2", "10", "--json")
	if code != 0 || diagnostic != "" {
		t.Fatalf("JSON comparison failed: %d %q %q", code, out, diagnostic)
	}
	var got, want map[string]any
	if json.Unmarshal([]byte(out), &got) != nil || json.Unmarshal([]byte(cliPassComparison), &want) != nil {
		t.Fatal("comparison output is not complete JSON")
	}
	gotBytes, _ := json.Marshal(got)
	wantBytes, _ := json.Marshal(want)
	if string(gotBytes) != string(wantBytes) || comparisons != 2 {
		t.Fatal("JSON comparison changed retained data or retried")
	}
}

func TestSwarmCompareCLIInvalidArgumentsNeverReachNetwork(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	for _, args := range [][]string{
		{"swarm", "compare"},
		{"swarm", "compare", "s1", "1", "s2"},
		{"swarm", "compare", "s1", "0", "s2", "10"},
		{"swarm", "compare", "s1", "01", "s2", "10"},
		{"swarm", "compare", "s1", "1", "s2", "1.5"},
		{"swarm", "compare", "s1", "1", "s2", "9223372036854775808"},
		{"swarm", "compare", "s1", "1", "s2", "10", "--json", "--json"},
		{"swarm", "compare", "s1", "1", "s2", "10", "--replay"},
		{"--guest", "swarm", "compare", "s1", "1", "s2", "10"},
	} {
		code, out, _ := invoke(t, "", args...)
		if code == 0 || out != "" {
			t.Fatalf("invalid comparison accepted: %v %d %q", args, code, out)
		}
	}
	if calls != 0 {
		t.Fatal("invalid comparison contacted server")
	}
}

func TestSwarmCompareCLIServerFailureDoesNotPrintPartialComparison(t *testing.T) {
	comparisons := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p","name":"demo"}]`)
		case "/api/projects/p/swarms":
			fmt.Fprint(w, `[{"id":"s1","name":"reference","state":"active"},{"id":"s2","name":"candidate","state":"active"}]`)
		case "/api/swarms/s1/passes/1/compare":
			comparisons++
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"detail":"synthetic-private-error"}`)
		default:
			t.Errorf("unexpected comparison request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	code, out, diagnostic := invoke(t, "", "swarm", "compare", "s1", "1", "s2", "10")
	if code != 1 || out != "" || !strings.Contains(diagnostic, "status 403") || strings.Contains(diagnostic, "synthetic-private-error") || comparisons != 1 {
		t.Fatalf("comparison refusal leaked/printed partial data: %d %q %q", code, out, diagnostic)
	}
}
