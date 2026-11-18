package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// FEATURE: HTTP-история проверяет текущую видимость роя, а не жизнь старого гостя.
// SPEC: UID/incarnation и отчёты сохраняются после reset; страницы metadata не копируют payload.
func TestChildHistoryAPIReadsStoppedHistoricalIncarnations(t *testing.T) {
	d := newChildEngineServer(t)
	args := childArgs(d, "history")
	args.Task = "Inspect <script>not executable</script>\nKeep the source bytes."
	child, err := d.srv.managed.createChild(context.Background(), d.master, args)
	if err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(context.Background())
	const report = "Independent child evidence <script>not executable</script>\nVerified."
	if err := d.srv.managed.completeChild(context.Background(), d.worker, child.ID, child.RequestID, "success", report); err != nil {
		t.Fatal(err)
	}
	r, b, _ := d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/stop", d.csrf(d.alice), nil)
	expectStatus(t, "stop", r, b, http.StatusOK)
	d.exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE swarm_id=?`, d.swarm)
	base := "/api/swarms/" + d.swarm + "/children"
	r, b, raw := d.do(d.alice, "GET", base+"?limit=1", "", nil)
	expectStatus(t, "history metadata", r, b, http.StatusOK)
	items, ok := b["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("bounded history metadata: %s", raw)
	}
	item := items[0].(map[string]any)
	if item["parentSequence"] != float64(1) || item["parentUid"] != d.master.UID || item["parentIncarnation"] != float64(1) || item["interrupted"] != true || item["registered"] != float64(1) || item["open"] != float64(1) || item["started"] != float64(1) || b["nextBefore"] != float64(0) {
		t.Fatalf("metadata rewrote original identity or readiness: %s", raw)
	}
	if strings.Contains(raw, "Keep the source bytes") || strings.Contains(raw, "Independent child evidence") {
		t.Fatal("metadata endpoint includes full payloads or reports")
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("private child history is cacheable")
	}
	r, b, raw = d.do(d.alice, "GET", base+"/1", "", nil)
	expectStatus(t, "historical group", r, b, http.StatusOK)
	children, ok := b["children"].([]any)
	if !ok || len(children) != 1 {
		t.Fatalf("historical tasks missing: %s", raw)
	}
	c := children[0].(map[string]any)
	if c["id"] != child.ID || c["executorUid"] != d.worker.UID || c["executorIncarnation"] != float64(1) || c["task"] != args.Task || c["report"] != report || c["state"] != "completed" || c["outcome"] != "success" || b["readiness"] != "unconfirmed" {
		t.Fatalf("historical group changed task evidence: %s", raw)
	}
	for _, forbidden := range []string{"SessionHash", "sessionHash", "credentialRef", "DelegatorID", "Caller"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatalf("internal authority field leaked: %s", forbidden)
		}
	}
	r, b, _ = d.do(d.alice, "GET", base+"?before=1&limit=1", "", nil)
	expectStatus(t, "older empty page", r, b, http.StatusOK)
	if values, ok := b["items"].([]any); !ok || len(values) != 0 || b["nextBefore"] != float64(0) {
		t.Fatalf("cursor repeated last group: %v", b)
	}
}

func TestChildHistoryAPICurrentVisibilityAndReadOnlyAccess(t *testing.T) {
	d := newChildEngineServer(t)
	if _, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "private-history")); err != nil {
		t.Fatal(err)
	}
	base := "/api/swarms/" + d.swarm + "/children"
	bob := newClient(t)
	d.login(bob, "bob", "bob-password-1")
	for _, path := range []string{base, base + "/1", base + "?limit=wrong"} {
		r, b, _ := d.do(bob, "GET", path, "", nil)
		expectStatus(t, "hidden swarm history", r, b, http.StatusNotFound)
	}
	r, b, _ := d.do(newClient(t), "GET", base, "", nil)
	expectStatus(t, "anonymous history", r, b, http.StatusUnauthorized)
	user, err := d.srv.store.GetUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	sw, err := d.srv.store.GetSwarm(context.Background(), d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	member, err := d.srv.store.IsMember(context.Background(), sw.ProjectID, user.ID)
	if err != nil || !member {
		t.Fatalf("outsider fixture must already share the project, not the swarm: %v %v", member, err)
	}
	if err := d.srv.store.SetSwarmMember(context.Background(), d.swarm, user.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	r, b, _ = d.do(bob, "GET", base+"/1", "", nil)
	expectStatus(t, "viewer reads history", r, b, http.StatusOK)
	r, b, _ = d.do(bob, "POST", base+"/1/abort", d.csrf(bob), map[string]any{"revision": 1, "confirm": true})
	expectStatus(t, "reading does not grant abort", r, b, http.StatusForbidden)
	if err := d.srv.store.RemoveSwarmMember(context.Background(), d.swarm, user.ID); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/1"} {
		r, b, _ = d.do(bob, "GET", base+suffix, "", nil)
		expectStatus(t, "revoked visibility", r, b, http.StatusNotFound)
	}
}

func TestChildHistoryAPIRejectsUnboundedOrAmbiguousQueries(t *testing.T) {
	d := newChildEngineServer(t)
	base := "/api/swarms/" + d.swarm + "/children"
	for _, query := range []string{"limit=", "before=", "limit=0", "limit=101", "limit=-1", "limit=1.5", "before=-1", "before=bad", "before=9223372036854775808", "limit=1&limit=2", "before=1&before=2", "all=true"} {
		t.Run(query, func(t *testing.T) {
			r, b, _ := d.do(d.alice, "GET", base+"?"+query, "", nil)
			expectStatus(t, "invalid history query", r, b, http.StatusBadRequest)
		})
	}
	for _, sequence := range []string{"0", "-1", "bad", "9223372036854775808"} {
		r, b, _ := d.do(d.alice, "GET", base+"/"+sequence, "", nil)
		expectStatus(t, "invalid history sequence", r, b, http.StatusBadRequest)
	}
	r, b, _ := d.do(d.alice, "GET", base+"/99", "", nil)
	expectStatus(t, "missing history group", r, b, http.StatusNotFound)
	r, b, _ = d.do(d.alice, "GET", "/api/swarms/missing-swarm/children/1", "", nil)
	expectStatus(t, "missing swarm", r, b, http.StatusNotFound)
}
