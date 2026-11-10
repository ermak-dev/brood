package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const swarmCLIRoles = `{"swarmId":"s3","execution":"idle","graph":{"pinned":false,"name":"release","version":3,"revision":"rev"},"roles":[{"role":"build","inGraph":true,"state":"ready","actorUid":"u1","actorName":"master","actorIncarnation":2},{"role":"extra","inGraph":false,"state":"missing","actorUid":"gone","actorIncarnation":1},{"role":"review","inGraph":true,"state":"unbound"}]}`

// FEATURE: bro swarm roles, bind and unbind (#235).
// SPEC: The swarm is named by ID or by a name unique among accessible
// swarms, the actor by managed name or UID of that swarm; bind sends the UID
// and the incarnation it resolved; arity errors never make a request.
// IMPLEMENTATION: Session client, swarm and actor resolution, tab output.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSwarmRolesCLI(t *testing.T) {
	var mutations []string
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"},{"id":"p2","name":"other"}]`)
		case r.URL.Path == "/api/projects/p1/swarms":
			fmt.Fprint(w, `[{"id":"s1","name":"main","state":"active"},{"id":"s9","name":"solo","state":"deleted"}]`)
		case r.URL.Path == "/api/projects/p2/swarms":
			fmt.Fprint(w, `[{"id":"s2","name":"main","state":"active"},{"id":"s3","name":"solo","state":"active"}]`)
		case r.URL.Path == "/api/swarms/s3/actors":
			fmt.Fprint(w, `{"results":[{"metadata":{"name":"native","uid":"n1"}},{"metadata":{"name":"master","uid":"u1"},"provider":"proxmox","generation":"2","phase":"running"}]}`)
		case r.URL.Path == "/api/swarms/s3/roles" && r.Method == "GET":
			if got := r.URL.Query().Get("definition"); got != "release" && got != "" {
				t.Errorf("definition %q", got)
			}
			fmt.Fprint(w, swarmCLIRoles)
		case r.URL.Path == "/api/swarms/s3/roles/build" && r.Method == "PUT":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if !reflect.DeepEqual(body, map[string]any{"actorUid": "u1", "actorIncarnation": float64(2)}) {
				t.Errorf("bind body %v", body)
			}
			mutations = append(mutations, "PUT build")
			fmt.Fprint(w, swarmCLIRoles)
		case r.URL.Path == "/api/swarms/s3/roles/review" && r.Method == "DELETE":
			mutations = append(mutations, "DELETE review")
			fmt.Fprint(w, swarmCLIRoles)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	table := "swarm s3 (solo): execution idle\n" +
		"graph: release version 3 revision rev (catalog)\n" +
		"ROLE\tGRAPH\tSTATE\tACTOR\tUID\tINCARNATION\n" +
		"build\tyes\tready\tmaster\tu1\t2\n" +
		"extra\tno\tmissing\t-\tgone\t1\n" +
		"review\tyes\tunbound\t-\t-\t-\n"
	for _, args := range [][]string{
		{"swarm", "roles", "solo", "--definition", "release"},
		{"swarm", "roles", "s3", "--definition=release"},
	} {
		code, out, diag := invoke(t, "", args...)
		if code != 0 || out != table || diag != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
	for _, actor := range []string{"master", "u1"} {
		code, out, diag := invoke(t, "", "swarm", "bind", "solo", "build", actor)
		if code != 0 || out != "bound build to master (uid u1, incarnation 2): ready\n" || diag != "" {
			t.Fatalf("bind %s: %d %q %q", actor, code, out, diag)
		}
	}
	code, out, diag := invoke(t, "", "swarm", "unbind", "s3", "review")
	if code != 0 || out != "unbound review\n" || diag != "" {
		t.Fatalf("unbind: %d %q %q", code, out, diag)
	}
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{"swarm", "roles", "main"}, "swarm name is ambiguous; use its ID"},
		{[]string{"swarm", "roles", "absent"}, "swarm not found or not accessible"},
		{[]string{"swarm", "bind", "s3", "build", "native"}, "actor not found in the swarm"},
		{[]string{"swarm", "bind", "s3", "build", "ghost"}, "actor not found in the swarm"},
	} {
		code, out, diag := invoke(t, "", tc.args...)
		if code != 1 || out != "" || !strings.Contains(diag, tc.reason) {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, diag)
		}
	}
	if !reflect.DeepEqual(mutations, []string{"PUT build", "PUT build", "DELETE review"}) {
		t.Fatalf("mutations: %v", mutations)
	}
}

func TestSwarmCLIRejectsArguments(t *testing.T) {
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid arguments made a request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, args := range [][]string{
		{"swarm"},
		{"swarm", "roles"},
		{"swarm", "roles", "s1", "extra"},
		{"swarm", "roles", "s1", "--definition"},
		{"swarm", "roles", "s1", "--definition="},
		{"swarm", "roles", "s1", "--definition", "a", "--definition", "b"},
		{"swarm", "bind", "s1", "build"},
		{"swarm", "bind", "s1", "build", "master", "extra"},
		{"swarm", "bind", "s1", "build", "master", "--definition", "a"},
		{"swarm", "unbind", "s1"},
		{"swarm", "unbind", "s1", "build", "master"},
		{"swarm", "start", "s1"},
		{"swarm", "roles", "s1", "--force"},
		{"swarm", "roles", ""},
	} {
		code, out, diag := invoke(t, "", args...)
		if code != 1 || out != "" || diag == "" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
	code, out, diag := invoke(t, "", "--guest", "swarm", "roles", "s1")
	if code != 1 || !strings.Contains(diag, "guest mode") {
		t.Fatalf("guest swarm: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "swarm", "--help")
	if code != 0 || !strings.Contains(out, "bro swarm bind SWARM ROLE ACTOR") || diag != "" {
		t.Fatalf("help: %d %q %q", code, out, diag)
	}
}
