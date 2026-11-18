package server

// BUG: Approved owner abort unnecessarily requires installation-specific policy.
// HYPOTHESIS: An optional provider hook is used as additional owner authority.

import (
	"context"
	"net/http"
	"testing"
)

func TestChildEngineOwnerStopRetainsIncompleteGroup(t *testing.T) {
	d := newChildEngineServer(t)
	child, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "one"))
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ := d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/stop", d.csrf(d.alice), nil)
	expectStatus(t, "owner stop with children", r, b, http.StatusOK)
	if d.committed().State != "unconfirmed" {
		t.Fatalf("stop incorrectly settled parent: %+v", d.committed())
	}
	retained, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || retained.Task.State != "pending" || retained.Task.Payload != child.Payload {
		t.Fatalf("stop erased or cancelled child intent: %+v %v", retained, err)
	}
}

func TestChildEngineAbortRequiresConfirmedOwnerWebSession(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	c, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "one"))
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/swarms/" + d.swarm + "/children/1/abort"
	body := map[string]any{"revision": 1, "confirm": true}
	r, b, _ := d.do(d.alice, "POST", path, "", body)
	expectStatus(t, "CSRF required", r, b, http.StatusForbidden)
	for _, bad := range []map[string]any{{"revision": 1, "confirm": false}, {"revision": 0, "confirm": true}} {
		r, b, _ = d.do(d.alice, "POST", path, d.csrf(d.alice), bad)
		expectStatus(t, "confirmation and revision required", r, b, http.StatusBadRequest)
	}
	r, b, _ = d.do(d.alice, "POST", path, d.csrf(d.alice), map[string]any{"revision": 7, "confirm": true})
	expectStatus(t, "revision fence", r, b, http.StatusConflict)
	// Recovery uses historical identity, not revoked worker authority. No
	// provider policy is needed beyond the explicitly confirmed owner action.
	d.exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, d.master.UID)
	r, b, _ = d.do(d.alice, "POST", path, d.csrf(d.alice), body)
	expectStatus(t, "explicit owner abort without provider policy", r, b, http.StatusOK)
	if b["state"] != "aborted" {
		t.Fatalf("abort: %v", b)
	}
	r, b, _ = d.do(d.alice, "POST", path, d.csrf(d.alice), body)
	expectStatus(t, "exact abort retry", r, b, http.StatusOK)
	w, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
	if err != nil || w.Task.State != "cancelled" || w.Task.Payload != c.Payload {
		t.Fatalf("abort erased history: %+v %v", w, err)
	}
}
