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

const statusReply = `{"id":"s/1","name":"build-flow","swarmId":"s/1","swarmName":"demo","projectId":"p","projectName":"project","owner":"operator","status":"waiting_human","definition":{"name":"build-flow","version":2,"revision":"rev"},"goal":"current goal","startedAt":"2026-11-18T00:00:00Z","currentNode":"ask","nodes":[{"id":"work","sequence":1,"kind":"actor","label":"work","status":"done","outcome":"success","actorName":"worker","report":"retained report","pass":1,"goal":"current goal","sourceCommit":"pinned-sha","commit":"attested-sha"},{"id":"ask","sequence":2,"kind":"human","label":"ask","status":"active","detail":"waiting reason","pass":1}],"gate":{"runId":"s/1","nodeId":"ask","visitSequence":2,"question":"Proceed?","kind":"multiple_choice","options":[{"id":"yes","label":"Yes"}]}}`

func TestSwarmStatusReadsActualContract(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.EscapedPath() != "/api/pipeline-runs/s%2F1" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "synthetic-session" {
			t.Error("missing session")
		}
		http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "rotated"})
		fmt.Fprint(w, statusReply)
	}))
	defer server.Close()
	client, _ := New(server.URL)
	client.Restore(StoredSession{Session: "synthetic-session"})
	got, err := client.SwarmStatus(context.Background(), "s/1")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(got)
	var want, actual any
	json.Unmarshal([]byte(statusReply), &want)
	json.Unmarshal(data, &actual)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("wire data changed: %s", data)
	}
	if calls != 1 || client.Stored().CSRF != "rotated" {
		t.Fatal("retried read or lost rotated cookie")
	}
}

func TestSwarmStatusAcceptsWireStatesAndLegacySourceAbsence(t *testing.T) {
	for _, state := range []string{"idle", "running", "waiting_human", "succeeded", "failed", "stopped"} {
		t.Run(state, func(t *testing.T) {
			body := strings.Replace(statusReply, `"status":"waiting_human"`, `"status":"`+state+`"`, 1)
			if state == "idle" {
				body = `{"id":"s/1","swarmId":"s/1","status":"idle","startedAt":null,"nodes":[]}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			client, _ := New(server.URL)
			got, err := client.SwarmStatus(context.Background(), "s/1")
			if err != nil || got.Status != state {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	for _, state := range []string{"pending", "active", "done", "failed", "completed", "unconfirmed", "refused", "stopped"} {
		t.Run("node-"+state, func(t *testing.T) {
			body := `{"id":"s/1","swarmId":"s/1","status":"stopped","startedAt":null,"currentNode":"work","nodes":[{"id":"work","sequence":7,"kind":"actor","label":"work","status":"` + state + `"}]}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			client, _ := New(server.URL)
			got, err := client.SwarmStatus(context.Background(), "s/1")
			if err != nil || len(got.Nodes) != 1 || got.Nodes[0].SourceCommit != "" {
				t.Fatalf("legacy reply: %+v %v", got, err)
			}
		})
	}
}

func TestSwarmStatusRejectsInvalidReplies(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, statusReply + ` {}`, `{"detail":"synthetic-private-material"}`,
		strings.Replace(statusReply, `"swarmId":"s/1"`, `"swarmId":"foreign"`, 1),
		strings.Replace(statusReply, `"id":"s/1"`, `"id":"foreign"`, 1),
		strings.Replace(statusReply, `"status":"waiting_human"`, `"status":"waiting"`, 1),
		strings.Replace(statusReply, `"status":"active"`, `"status":"invented"`, 1),
		strings.Replace(statusReply, `"currentNode":"ask"`, `"currentNode":"missing"`, 1),
		strings.Replace(statusReply, `"sequence":2`, `"sequence":1`, 1),
		strings.Replace(statusReply, `"sequence":1`, `"sequence":0`, 1),
		`{"id":"s/1","swarmId":"s/1","status":"running","nodes":[]}`,
		strings.Repeat(" ", (32<<20)+1),
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		client, _ := New(server.URL)
		got, err := client.SwarmStatus(context.Background(), "s/1")
		server.Close()
		if err == nil || !reflect.DeepEqual(got, SwarmStatus{}) || strings.Contains(err.Error(), "synthetic-private-material") {
			t.Fatalf("invalid reply accepted/leaked: %+v %v", got, err)
		}
	}
}

func TestSwarmStatusRefusalsAndInvalidSelection(t *testing.T) {
	for _, code := range []int{401, 403, 404, 500, 302} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(code)
			fmt.Fprint(w, `{"detail":"synthetic-private-material"}`)
		}))
		client, _ := New(server.URL)
		for _, id := range []string{"", strings.Repeat("s", 257)} {
			if _, err := client.SwarmStatus(context.Background(), id); err == nil {
				t.Fatal("invalid ID accepted")
			}
		}
		if calls != 0 {
			t.Fatal("invalid ID contacted server")
		}
		got, err := client.SwarmStatus(context.Background(), "s/1")
		server.Close()
		if err == nil || strings.Contains(err.Error(), "synthetic-private-material") || calls != 1 || !reflect.DeepEqual(got, SwarmStatus{}) {
			t.Fatalf("unsafe refusal: %+v %v calls=%d", got, err, calls)
		}
		if code == 401 && !errors.Is(err, ErrSessionExpired) {
			t.Fatal("lost session error")
		}
		if code == 302 && !errors.Is(err, ErrRedirect) {
			t.Fatal("lost redirect error")
		}
	}
}
