package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func comparisonReply(reference string, referencePass int64, candidate string, candidatePass int64) map[string]any {
	return map[string]any{
		"version": 1, "alignment": "visit-position",
		"reference": map[string]any{"swarmId": reference, "pass": referencePass},
		"candidate": map[string]any{"swarmId": candidate, "pass": candidatePass},
		"sameGoal":  true, "sameSource": true, "sameDefinition": false,
		"stepCount":      map[string]any{"reference": 1, "candidate": 1, "delta": 0},
		"childTaskCount": map[string]any{"reference": 0, "candidate": 0, "delta": 0},
		"steps": []any{map[string]any{"position": 1, "node": "same", "outcome": "unknown", "report": "unknown", "reportedOutcome": "unknown", "reportedReport": "unknown",
			"reference": map[string]any{"sequence": referencePass, "nodeId": "build", "state": "inflight", "reportedOutcome": "success", "reportedReport": "Reported, not applied.", "childTasks": 0},
			"candidate": map[string]any{"sequence": candidatePass, "nodeId": "build", "state": "completed", "outcome": "fail", "report": "Actual retained failure.", "childTasks": 0}}},
	}
}

// Клиент сравнения (#293) читает только выбранную пару и не пересылает
// cookie через redirect. Неизвестные результаты остаются неизвестными.
func TestPassComparisonClientExactScopeAndLargeReports(t *testing.T) {
	calls := 0
	reply := comparisonReply("source/one", 1, "other & swarm", 19)
	report := strings.Repeat("retained report. ", 100000)
	reply["steps"].([]any)[0].(map[string]any)["candidate"].(map[string]any)["report"] = report
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.EscapedPath() != "/api/swarms/source%2Fone/passes/1/compare" || r.URL.Query().Get("swarm") != "other & swarm" || r.URL.Query().Get("pass") != "19" || len(r.URL.Query()) != 2 {
			t.Errorf("wrong comparison request: %s %s", r.Method, r.URL)
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "synthetic-session" {
			t.Error("comparison lost scoped human session")
		}
		http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "rotated-synthetic-csrf"})
		json.NewEncoder(w).Encode(reply)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Restore(StoredSession{Session: "synthetic-session", CSRF: "synthetic-csrf"})
	got, err := client.CompareSwarmPasses(context.Background(), "source/one", 1, "other & swarm", 19)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.SwarmID != "source/one" || got.Candidate.Pass != 19 || len(got.Steps) != 1 || got.Steps[0].Reference.Outcome != nil || got.Steps[0].Reference.ReportedOutcome == nil || *got.Steps[0].Reference.ReportedOutcome != "success" || got.Steps[0].Candidate.Report == nil || *got.Steps[0].Candidate.Report != report {
		t.Fatal("comparison truncated reports or conflated reported/actual results")
	}
	if client.Stored().CSRF != "rotated-synthetic-csrf" || calls != 1 {
		t.Fatal("cookie rotation lost or comparison automatically retried")
	}
}

func TestPassComparisonClientRejectsInvalidSelectionBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected request", 500) }))
	defer server.Close()
	client, _ := New(server.URL)
	for _, selection := range []struct {
		reference, candidate string
		left, right          int64
	}{
		{"", "other", 1, 1}, {"reference", "", 1, 1}, {"reference", "other", 0, 1}, {"reference", "other", 1, -1}, {strings.Repeat("s", 257), "other", 1, 1},
	} {
		got, err := client.CompareSwarmPasses(context.Background(), selection.reference, selection.left, selection.candidate, selection.right)
		if err == nil || !reflect.DeepEqual(got, SwarmPassComparison{}) {
			t.Fatal("invalid selection returned a comparison")
		}
	}
	if calls != 0 {
		t.Fatal("invalid selection reached network")
	}
}

func TestPassComparisonClientRejectsForeignMalformedOversizedAndTrailingReplies(t *testing.T) {
	valid := comparisonReply("reference", 1, "candidate", 10)
	data, _ := json.Marshal(valid)
	foreign := comparisonReply("foreign", 1, "candidate", 10)
	foreignData, _ := json.Marshal(foreign)
	malformed := comparisonReply("reference", 1, "candidate", 10)
	malformed["steps"].([]any)[0].(map[string]any)["position"] = 0
	malformedData, _ := json.Marshal(malformed)
	for _, body := range []string{string(foreignData), string(malformedData), `{}`, string(data) + ` {"extra":true}`, strings.Repeat(" ", MaxPassComparisonBytes+2) + string(data), strings.Repeat(" ", MaxPassComparisonBytes+1-len(data)) + string(data)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		client, _ := New(server.URL)
		got, err := client.CompareSwarmPasses(context.Background(), "reference", 1, "candidate", 10)
		server.Close()
		if err == nil || !reflect.DeepEqual(got, SwarmPassComparison{}) {
			t.Fatal("invalid server data returned partial comparison")
		}
	}
}

func TestPassComparisonClientRefusesRedirectAndKeepsPrivateErrorBodyOutOfOutput(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusFound} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"detail":"private-synthetic-error-material"}`)
		}))
		client, _ := New(server.URL)
		_, err := client.CompareSwarmPasses(context.Background(), "reference", 1, "candidate", 10)
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private-synthetic-error-material") || calls != 1 {
			t.Fatal("server refusal leaked private body or triggered another request")
		}
		if status == http.StatusUnauthorized && !errors.Is(err, ErrSessionExpired) {
			t.Fatal("session refusal lost its type")
		}
		if status == http.StatusFound && !errors.Is(err, ErrRedirect) {
			t.Fatal("redirect refusal lost its type")
		}
	}
}
