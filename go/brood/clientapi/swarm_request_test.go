package clientapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPolicyRequestClientPreservesRevisionAndUsesScopedCSRFTransport(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.Method {
		case http.MethodGet:
			if r.URL.EscapedPath() != "/api/swarms/s%2F1/requests" || r.URL.Query().Get("all") != "1" {
				t.Errorf("wrong request listing: %s", r.URL)
			}
			fmt.Fprint(w, `{"results":[{"id":"policy:candidate","swarmId":"s/1","origin":"policy","kind":"confirm","state":"open","revision":1}]}`)
		case http.MethodPost:
			if r.URL.EscapedPath() != "/api/swarms/s%2F1/requests/policy:candidate/answer" || r.Header.Get("X-CSRFToken") != "csrf" {
				t.Errorf("unscoped/unprotected answer: %s", r.URL)
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["revision"] != float64(1) || body["response"] != "yes" || len(body) != 2 {
				t.Errorf("answer lost exact revision or gained graph outcome: %+v", body)
			}
			fmt.Fprint(w, `{"id":"policy:candidate","swarmId":"s/1","origin":"policy","kind":"confirm","state":"answered","revision":2,"response":"yes"}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	requests, err := client.SwarmRequests(context.Background(), "s/1", true)
	if err != nil || len(requests) != 1 || requests[0].Revision != 1 || requests[0].Origin != "policy" {
		t.Fatalf("policy list: %+v %v", requests, err)
	}
	for _, revision := range []int64{0, 2} {
		if _, err := client.AnswerSwarmRequest(context.Background(), "s/1", "policy:candidate", SwarmRequestAnswer{Response: "yes", Revision: revision}); err == nil {
			t.Fatalf("client inferred/ignored invalid revision %d", revision)
		}
	}
	answer, err := client.AnswerSwarmRequest(context.Background(), "s/1", "policy:candidate", SwarmRequestAnswer{Response: "yes", Revision: 1})
	if err != nil || answer.Revision != 2 || answer.State != "answered" || answer.Delivery != "" {
		t.Fatalf("policy answer: %+v %v", answer, err)
	}
	if calls != 2 {
		t.Fatalf("invalid inputs reached server or transport retried: calls=%d", calls)
	}
}
