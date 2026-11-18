package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const cliSwarmStatus = `{"id":"s1","name":"workflow","swarmId":"s1","swarmName":"demo","projectId":"p","projectName":"project","owner":"operator","status":"running","definition":{"name":"workflow","version":2,"revision":"rev"},"goal":"operator task","startedAt":null,"currentNode":"work","nodes":[{"id":"work","sequence":1,"kind":"actor","label":"work","status":"refused","detail":"source mismatch\u001b[31m","report":"saved\nreport","pass":1,"goal":"operator task","sourceCommit":"pinned-sha","commit":"attested-sha"}]}`

func statusFixture(t *testing.T, reply string, code int, swarms string) *int {
	t.Helper()
	reads := new(int)
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("status wrote data: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p","name":"project"}]`)
		case "/api/projects/p/swarms":
			fmt.Fprint(w, swarms)
		case "/api/pipeline-runs/s1":
			*reads++
			w.WriteHeader(code)
			fmt.Fprint(w, reply)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
	return reads
}

func TestSwarmStatusCLITextAndJSON(t *testing.T) {
	reads := statusFixture(t, cliSwarmStatus, 200, `[{"id":"s1","name":"demo","state":"active"}]`)
	code, out, diagnostic := invoke(t, "", "swarm", "status", "demo")
	if code != 0 || diagnostic != "" {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	for _, part := range []string{`swarm "s1" ("demo"): running`, `graph: "workflow" version 2 revision "rev"`, `goal: "operator task"`, `current node: "work"`, `visit 1 "work": refused`, `pass 1`, `source commit: "pinned-sha"`, `attested commit: "attested-sha"`, `detail: "source mismatch\x1b[31m"`, `report: "saved\nreport"`} {
		if !strings.Contains(out, part) {
			t.Errorf("missing %q: %s", part, out)
		}
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Fatal("unescaped terminal control")
	}
	code, out, diagnostic = invoke(t, "", "swarm", "status", "s1", "--json")
	var got, want any
	if code != 0 || diagnostic != "" || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("JSON failed: %d %q %q", code, out, diagnostic)
	}
	json.Unmarshal([]byte(cliSwarmStatus), &want)
	if !reflect.DeepEqual(got, want) || *reads != 2 {
		t.Fatalf("JSON changed or read retried: %s reads=%d", out, *reads)
	}
}

func TestSwarmStatusCLIIdleAndLegacy(t *testing.T) {
	for _, reply := range []string{
		`{"id":"s1","swarmId":"s1","status":"idle","nodes":[],"startedAt":null}`,
		`{"id":"s1","swarmId":"s1","status":"waiting_human","currentNode":"ask","nodes":[{"id":"ask","sequence":1,"kind":"human","label":"ask","status":"active"}],"startedAt":null}`,
	} {
		t.Run(reply, func(t *testing.T) {
			statusFixture(t, reply, 200, `[{"id":"s1","name":"demo","state":"active"}]`)
			code, out, diagnostic := invoke(t, "", "swarm", "status", "s1")
			if code != 0 || diagnostic != "" || !strings.Contains(out, "source commit: unknown") {
				t.Fatalf("%d %q %q", code, out, diagnostic)
			}
			if strings.Contains(reply, `"idle"`) && !strings.Contains(out, "no retained visits") {
				t.Fatal(out)
			}
			if strings.Contains(reply, "waiting_human") && !strings.Contains(out, "waiting_human") {
				t.Fatal(out)
			}
		})
	}
}

func TestSwarmStatusCLIInvalidArgumentsAndGuest(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	for _, args := range [][]string{
		{"swarm", "status"}, {"swarm", "status", ""}, {"swarm", "status", "s1", "extra"},
		{"swarm", "status", "s1", "--json", "--json"}, {"swarm", "status", "s1", "--json=true"},
		{"swarm", "status", "s1", "--start"}, {"swarm", "status", "s1", "--commit", "sha"},
		{"--guest", "swarm", "status", "s1"},
	} {
		code, out, _ := invoke(t, "", args...)
		if code == 0 || out != "" {
			t.Fatalf("accepted %v: %d %q", args, code, out)
		}
	}
	if calls != 0 {
		t.Fatal("invalid input reached server")
	}
}

func TestSwarmStatusCLIRefusals(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500, 302, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			reads := statusFixture(t, `{"detail":"synthetic-private-error"}`, status, `[{"id":"s1","name":"demo","state":"active"}]`)
			code, out, diagnostic := invoke(t, "", "swarm", "status", "s1", "--json")
			want := 1
			if status == 401 {
				want = 2
			}
			if code != want || out != "" || diagnostic == "" || strings.Contains(diagnostic, "synthetic-private-error") || *reads != 1 {
				t.Fatalf("%d %q %q reads=%d", code, out, diagnostic, *reads)
			}
		})
	}
}

func TestSwarmStatusCLINameResolution(t *testing.T) {
	for _, tc := range []struct{ selection, swarms, reason string }{
		{"missing", `[{"id":"s1","name":"demo"}]`, "not found"},
		{"demo", `[{"id":"s1","name":"demo"},{"id":"s2","name":"demo"}]`, "ambiguous"},
		{"demo", `[{"id":"s1","name":"demo","state":"deleted"}]`, "not found"},
	} {
		t.Run(tc.reason+tc.swarms, func(t *testing.T) {
			reads := statusFixture(t, cliSwarmStatus, 200, tc.swarms)
			code, out, diagnostic := invoke(t, "", "swarm", "status", tc.selection)
			if code != 1 || out != "" || !strings.Contains(diagnostic, tc.reason) || *reads != 0 {
				t.Fatalf("%d %q %q reads=%d", code, out, diagnostic, *reads)
			}
		})
	}
}

func TestSwarmStatusCLIHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"swarm", "--help"}, {"swarm", "status", "--help"}} {
		code, out, diagnostic := invoke(t, "", args...)
		if code != 0 || diagnostic != "" || !strings.Contains(out, "swarm status SWARM [--json]") {
			t.Fatalf("help %v: %d %q %q", args, code, out, diagnostic)
		}
	}
}
