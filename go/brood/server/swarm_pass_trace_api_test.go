package server

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

func passTraceAPIFixture(t *testing.T) *resultCollectionFixture {
	t.Helper()
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("trace fixture did not retain acknowledged completion")
	}
	f.srv.steps.wait()
	return f
}

func TestPassTraceAPIExportsExactHeldResultWithoutRootAuthorityMaterial(t *testing.T) {
	f := passTraceAPIFixture(t)
	ctx := context.Background()
	path := "/api/swarms/" + f.swarm + "/passes/1"
	r, body, raw := f.do(f.alice, "GET", path, "", nil)
	expectStatus(t, "owner pass trace", r, body, http.StatusOK)
	if body["version"] != float64(1) || body["swarmId"] != f.swarm || body["pass"] != float64(1) || body["current"] != true || body["execution"] != "running" {
		t.Fatalf("trace header lost exact pass state: %s", raw)
	}
	swarm, err := f.srv.store.GetSwarm(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.srv.store.GetSwarmStep(ctx, f.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if body["goal"] != first.Goal || body["graph"] != string(swarm.Graph) || body["source"].(map[string]any)["commit"] != first.SourceCommit {
		t.Fatal("export rewrote graph/goal or borrowed source from another pass")
	}
	steps, ok := body["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("trace did not export the exact single visit: %s", raw)
	}
	step := steps[0].(map[string]any)
	if step["sequence"] != float64(1) || step["state"] != "inflight" || step["outcome"] != nil || step["reportedResult"].(map[string]any)["outcome"] != "success" || step["resultObservation"].(map[string]any)["state"] != "observed" {
		t.Fatalf("export conflated actual outcome and reported/evidenced work: %s", raw)
	}
	artifact := step["artifactEvidence"].(map[string]any)
	if artifact["commit"] != strings.Repeat("a", 40) || artifact["producerUid"] != f.parent.UID || !reflect.DeepEqual(artifact["paths"], []any{"go/result.go"}) {
		t.Fatalf("export dropped exact verified artifact material: %v", artifact)
	}
	for _, forbidden := range []string{"lease", "Lease", "Caller", "caller", "challenge", "Challenge", "signature", "Signature", "nonce", "Nonce", "token", "sessionHash", "SessionHash", "credentialRef"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatalf("root authority field leaked through export: %s", forbidden)
		}
	}
	if r.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatal("private trace export is cacheable or not JSON")
	}
	r, downloaded, _ := f.do(f.alice, "GET", path+"?download=1", "", nil)
	expectStatus(t, "download complete trace", r, downloaded, http.StatusOK)
	if !reflect.DeepEqual(body, downloaded) || r.Header.Get("Content-Disposition") != `attachment; filename="brood-pass-1.json"` {
		t.Fatal("download changed trace material or accepted an uncontrolled filename")
	}
	if f.calls.Load() != 1 {
		t.Fatal("reading/exporting a trace repeated workspace observation")
	}
}

func TestPassTraceAPIChecksCurrentVisibilityAndNeverGrantsMutation(t *testing.T) {
	f := passTraceAPIFixture(t)
	path := "/api/swarms/" + f.swarm + "/passes/1"
	bob := newClient(t)
	f.login(bob, "bob", "bob-password-1")
	for _, suffix := range []string{"", "?download=1", "?unknown=1"} {
		r, body, _ := f.do(bob, "GET", path+suffix, "", nil)
		expectStatus(t, "hidden trace", r, body, http.StatusNotFound)
	}
	r, body, _ := f.do(newClient(t), "GET", path, "", nil)
	expectStatus(t, "anonymous trace", r, body, http.StatusUnauthorized)
	user, err := f.srv.store.GetUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetSwarmMember(context.Background(), f.swarm, user.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	r, body, _ = f.do(bob, "GET", path, "", nil)
	expectStatus(t, "viewer trace", r, body, http.StatusOK)
	r, body, _ = f.do(bob, "POST", path, f.csrf(bob), map[string]any{})
	expectStatus(t, "read-only trace", r, body, http.StatusMethodNotAllowed)
	if err := f.srv.store.RemoveSwarmMember(context.Background(), f.swarm, user.ID); err != nil {
		t.Fatal(err)
	}
	r, body, _ = f.do(bob, "GET", path+"?download=1", "", nil)
	expectStatus(t, "revoked trace export", r, body, http.StatusNotFound)
	if f.calls.Load() != 1 {
		t.Fatal("trace access attempted an actor operation")
	}
}

func TestPassTraceAPIDoesNotAcceptGuestIdentityAsHumanTraceAccess(t *testing.T) {
	f := passTraceAPIFixture(t)
	request, err := http.NewRequest("GET", f.http.URL+"/api/swarms/"+f.swarm+"/passes/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+guestToken(f.srv.managed.registry.relaySecret, f.parent.UID, f.parent.Incarnation))
	response, err := newClient(t).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("authenticated guest identity became human trace-read authority")
	}
}

func TestPassTraceAPIKeepsHistoricalArtifactIdentityAfterStopAndIncarnationChange(t *testing.T) {
	f := passTraceAPIFixture(t)
	r, body, _ := f.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/stop", f.csrf(f.alice), nil)
	expectStatus(t, "stop before historical export", r, body, http.StatusOK)
	f.exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, f.parent.UID)
	r, body, raw := f.do(f.alice, "GET", "/api/swarms/"+f.swarm+"/passes/1?download=1", "", nil)
	expectStatus(t, "historical trace after incarnation change", r, body, http.StatusOK)
	step := body["steps"].([]any)[0].(map[string]any)
	actor := step["actor"].(map[string]any)
	artifact := step["artifactEvidence"].(map[string]any)
	if actor["uid"] != f.parent.UID || actor["incarnation"] != float64(1) || artifact["producerUid"] != f.parent.UID || artifact["incarnation"] != float64(1) || artifact["commit"] != strings.Repeat("a", 40) || f.calls.Load() != 1 {
		t.Fatalf("historical export rebound evidence or retried the old guest: %s", raw)
	}
}

func TestPassTraceAPIRejectsAmbiguousPassesAndExportQueries(t *testing.T) {
	f := passTraceAPIFixture(t)
	base := "/api/swarms/" + f.swarm + "/passes/"
	for _, pass := range []string{"0", "-1", "01", "bad", "9223372036854775808"} {
		r, body, _ := f.do(f.alice, "GET", base+pass, "", nil)
		expectStatus(t, "invalid pass", r, body, http.StatusBadRequest)
	}
	for _, query := range []string{"download=", "download=wrong", "download=1&download=0", "filename=elsewhere", "all=1"} {
		r, body, _ := f.do(f.alice, "GET", base+"1?"+query, "", nil)
		expectStatus(t, "invalid export query", r, body, http.StatusBadRequest)
	}
	r, body, _ := f.do(f.alice, "GET", base+"99", "", nil)
	expectStatus(t, "missing pass", r, body, http.StatusNotFound)
}

func TestPassTraceAPIReturnsExplicitBudgetRefusalWithoutPartialJSON(t *testing.T) {
	f := passTraceAPIFixture(t)
	f.srv.steps.close()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for sequence := 2; sequence <= store.MaxPassTraceRecords+1; sequence++ {
		if _, err := tx.Exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES(?,?,'build','pending')`, f.swarm, sequence); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r, body, raw := f.do(f.alice, "GET", "/api/swarms/"+f.swarm+"/passes/1?download=1", "", nil)
	expectStatus(t, "export budget", r, body, http.StatusRequestEntityTooLarge)
	if body["steps"] != nil || !strings.Contains(raw, "no partial trace") || r.Header.Get("Content-Disposition") != "" {
		t.Fatal("export returned a truncated document or download header after refusal")
	}
}
