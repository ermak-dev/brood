package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// Сравнение (#293) только читает две видимые трассы. Доступ к одной стороне
// не открывает другую и не разрешает повторное выполнение ни одной из них.
func comparisonAPIFixture(t *testing.T) (*resultCollectionFixture, string) {
	t.Helper()
	f := passTraceAPIFixture(t)
	alice, err := f.srv.store.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetUserAdmin(context.Background(), alice.ID, false); err != nil {
		t.Fatal(err)
	}
	f.login(f.alice, "alice", "alice-password-1")
	other, err := f.srv.store.GetSwarm(context.Background(), f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := f.srv.store.GetUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	other.ID, other.Name, other.AtespaceName, other.AtespaceUID, other.OwnerID = store.NewID(), "comparison-other", "comparison-other", nil, bob.ID
	if err := f.srv.store.CreateSwarm(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,goal,source_commit,outcome,report) VALUES(?,7,'build','completed','Other retained goal',?,'fail','Other retained result')`, other.ID, strings.Repeat("b", 40))
	return f, other.ID
}

func comparisonAPIPath(swarm, pass, other, otherPass string) string {
	return "/api/swarms/" + url.PathEscape(swarm) + "/passes/" + pass + "/compare?swarm=" + url.QueryEscape(other) + "&pass=" + url.QueryEscape(otherPass)
}

func TestPassComparisonAPIRequiresVisibilityOfBothSides(t *testing.T) {
	f, other := comparisonAPIFixture(t)
	ctx := context.Background()
	path := comparisonAPIPath(f.swarm, "1", other, "7")
	r, body, _ := f.do(f.alice, "GET", path, "", nil)
	expectStatus(t, "candidate not granted", r, body, http.StatusNotFound)
	bob := newClient(t)
	f.login(bob, "bob", "bob-password-1")
	r, body, _ = f.do(bob, "GET", path, "", nil)
	expectStatus(t, "reference not granted", r, body, http.StatusNotFound)
	alice, err := f.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetSwarmMember(ctx, other, alice.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	r, body, raw := f.do(f.alice, "GET", path, "", nil)
	expectStatus(t, "both sides visible", r, body, http.StatusOK)
	if body["alignment"] != "visit-position" || body["sameGoal"] != false || body["sameSource"] != false {
		t.Fatalf("comparison context lost: %s", raw)
	}
	rows := body["steps"].([]any)
	row := rows[0].(map[string]any)
	if len(rows) != 1 || row["outcome"] != "unknown" || row["reference"].(map[string]any)["reportedOutcome"] != "success" || row["reference"].(map[string]any)["outcome"] != nil || row["candidate"].(map[string]any)["outcome"] != "fail" {
		t.Fatalf("reported and actual conflated: %s", raw)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("private comparison is cacheable")
	}
	for _, forbidden := range []string{"Lease", "lease", "challenge", "signature", "nonce", "sessionHash", "credentialRef"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatal("comparison leaked authority field", forbidden)
		}
	}
	r, downloaded, _ := f.do(f.alice, "GET", path+"&download=1", "", nil)
	expectStatus(t, "comparison download", r, downloaded, http.StatusOK)
	if !reflect.DeepEqual(body, downloaded) || r.Header.Get("Content-Disposition") != `attachment; filename="brood-pass-comparison.json"` {
		t.Fatal("download changed comparison or filename")
	}
	if err := f.srv.store.RemoveSwarmMember(ctx, other, alice.ID); err != nil {
		t.Fatal(err)
	}
	r, body, _ = f.do(f.alice, "GET", path, "", nil)
	expectStatus(t, "candidate access revoked", r, body, http.StatusNotFound)
	if f.calls.Load() != 1 {
		t.Fatal("comparison repeated workspace observation")
	}
}

func TestPassComparisonAPIBudgetAndMachineIdentity(t *testing.T) {
	f := passTraceAPIFixture(t)
	path := comparisonAPIPath(f.swarm, "1", f.swarm, "1")
	request, err := http.NewRequest("GET", f.http.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+guestToken(f.srv.managed.registry.relaySecret, f.parent.UID, f.parent.Incarnation))
	response, err := newClient(t).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("guest identity became human comparison authority")
	}
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
	r, body, raw := f.do(f.alice, "GET", path+"&download=1", "", nil)
	expectStatus(t, "oversized comparison", r, body, http.StatusRequestEntityTooLarge)
	if body["steps"] != nil || !strings.Contains(raw, "no partial comparison") || r.Header.Get("Content-Disposition") != "" {
		t.Fatal("oversized comparison returned partial data or a download header")
	}
	if f.calls.Load() != 1 {
		t.Fatal("comparison budget refusal caused a guest effect")
	}
}

func TestPassComparisonAPIIsReadOnlyAndRejectsAmbiguousSelection(t *testing.T) {
	f := passTraceAPIFixture(t)
	path := comparisonAPIPath(f.swarm, "1", f.swarm, "1")
	r, body, _ := f.do(newClient(t), "GET", path, "", nil)
	expectStatus(t, "anonymous comparison", r, body, http.StatusUnauthorized)
	r, body, _ = f.do(f.alice, "POST", path, f.csrf(f.alice), map[string]any{})
	expectStatus(t, "comparison mutation", r, body, http.StatusMethodNotAllowed)
	if r.Header.Get("Allow") != "GET, HEAD" {
		t.Fatal("read-only methods missing")
	}
	for _, bad := range []string{
		comparisonAPIPath(f.swarm, "01", f.swarm, "1"),
		comparisonAPIPath(f.swarm, "1", f.swarm, "01"),
		comparisonAPIPath(f.swarm, "1", f.swarm, "0"),
		comparisonAPIPath(f.swarm, "1", f.swarm, "9223372036854775808"),
		path + "&pass=2", path + "&swarm=foreign", path + "&unknown=1", path + "&download=", path + "&download=1&download=0",
		"/api/swarms/" + f.swarm + "/passes/1/compare?swarm=" + f.swarm,
	} {
		r, body, _ = f.do(f.alice, "GET", bad, "", nil)
		expectStatus(t, "invalid comparison selection", r, body, http.StatusBadRequest)
	}
	r, body, _ = f.do(f.alice, "GET", comparisonAPIPath(f.swarm, "1", f.swarm, "99"), "", nil)
	expectStatus(t, "missing candidate pass", r, body, http.StatusNotFound)
	r, body, _ = f.do(f.alice, "GET", comparisonAPIPath(f.swarm, "99", f.swarm, "1"), "", nil)
	expectStatus(t, "missing reference pass", r, body, http.StatusNotFound)
	if f.calls.Load() != 1 {
		t.Fatal("comparison caused a guest effect")
	}
}
