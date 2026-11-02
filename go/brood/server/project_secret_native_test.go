package server

import (
	"context"
	"strings"
	"testing"
)

// FEATURE: Native actors must not silently omit project secrets or embed them in readable templates.
// SPEC: Only a provider with authenticated private delivery may start a secret-bearing project.
func TestNativeActorSecretCapabilityRefusal(t *testing.T) {
	ts, admin, master, _ := masterSetup(t)
	sw, _ := ts.srv.store.GetSwarm(context.Background(), master.SwarmID)
	if err := ts.srv.store.PutProjectSecretVars(context.Background(), sw.ProjectID, map[string]string{"APP_TOKEN": "synthetic-private-token"}); err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(admin, "POST", "/api/swarms/"+master.SwarmID+"/templates", ts.csrf(admin), map[string]any{
		"metadata":       map[string]string{"name": "native-test-template"},
		"containers":     []any{map[string]string{"name": "main", "image": "busybox"}},
		"resources":      map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1048576"}},
		"snapshotConfig": map[string]any{"goldenSnapshotConfig": map[string]string{"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}},
	})
	expectStatus(t, "template", r, b, 201)
	r, _, raw := ts.do(admin, "POST", "/api/swarms/"+master.SwarmID+"/actors", ts.csrf(admin), map[string]any{"metadata": map[string]string{"name": "native-secret-test"}, "actorTemplate": map[string]string{"name": "native-test-template"}})
	if r.StatusCode != 409 || !strings.Contains(raw, "private project-secret delivery") || strings.Contains(raw, "synthetic-private-token") {
		t.Fatalf("unsupported native delivery was not refused: %d %s", r.StatusCode, raw)
	}
}
