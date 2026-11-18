package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSwarmPolicyRequestCLIListsAndAnswersExactRevision(t *testing.T) {
	answers := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p","name":"demo"}]`)
		case "/api/projects/p/swarms":
			fmt.Fprint(w, `[{"id":"s","name":"main","state":"active"}]`)
		case "/api/swarms/s/requests":
			fmt.Fprint(w, `{"results":[{"id":"policy:candidate","swarmId":"s","origin":"policy","kind":"confirm","state":"open","revision":1,"question":"Approve automatic transition?"}]}`)
		case "/api/swarms/s/requests/policy:candidate/answer":
			answers++
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["response"] != "yes" || body["revision"] != float64(1) || len(body) != 2 {
				t.Errorf("wrong answer body: %+v", body)
			}
			fmt.Fprint(w, `{"id":"policy:candidate","swarmId":"s","origin":"policy","kind":"confirm","state":"answered","revision":2,"response":"yes"}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	code, out, diagnostic := invoke(t, "", "swarm", "requests", "main")
	if code != 0 || !strings.Contains(out, "policy:candidate") || !strings.Contains(out, "policy") || !strings.Contains(out, "1") || !strings.Contains(out, "Approve automatic transition?") || diagnostic != "" {
		t.Fatalf("request list: code=%d out=%q diagnostic=%q", code, out, diagnostic)
	}
	code, out, diagnostic = invoke(t, "", "swarm", "answer", "main", "policy:candidate", "--response", "yes", "--revision", "1")
	if code != 0 || !strings.Contains(out, "recorded") || strings.Contains(out, "advanced") || diagnostic != "" || answers != 1 {
		t.Fatalf("policy answer: code=%d answers=%d out=%q diagnostic=%q", code, answers, out, diagnostic)
	}
}

func TestSwarmPolicyRequestCLIInvalidArgumentsNeverReachNetwork(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	for _, args := range [][]string{
		{"--guest", "swarm", "requests", "s"},
		{"swarm", "answer", "s", "policy:c", "--response", "yes"},
		{"swarm", "answer", "s", "policy:c", "--response", "yes", "--revision", "2"},
		{"swarm", "answer", "s", "policy:c", "--response", "maybe", "--revision", "1"},
		{"swarm", "answer", "s", "policy:c", "--response", "yes", "--revision", "1", "--revision", "1"},
		{"swarm", "requests", "s", "--revision", "1"},
	} {
		if code, _, _ := invoke(t, "", args...); code == 0 {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid input caused %d network calls", calls)
	}
}
