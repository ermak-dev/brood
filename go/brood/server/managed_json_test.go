package server

// FEATURE: Mixed native/managed JSON list compatibility.
// SPEC: Retain results envelopes and canonical camelCase decimal-string int64
// metadata, including exact values beyond JavaScript's integer precision.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func TestManagedListPreservesNativeJSON(t *testing.T) {
	w := httptest.NewRecorder()
	native := []*ateapi.Actor{{Metadata: &ateapi.ResourceMetadata{Name: "native", Uid: "native-uid", Version: 9007199254740993}, Status: &ateapi.ActorStatus{}}}
	managed := []any{map[string]any{"metadata": map[string]any{"name": "managed", "uid": "managed-uid", "version": "9007199254740995"}, "provider": "proxmox", "placement": map[string]any{"node": "node-a", "ctid": 105, "pool": "pool"}}}
	writeManagedList(w, native, managed)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	rows := body["results"].([]any)
	if len(rows) != 2 {
		t.Fatalf("mixed inventory missing: %s", w.Body.String())
	}
	for i, want := range []string{"9007199254740993", "9007199254740995"} {
		if rows[i].(map[string]any)["metadata"].(map[string]any)["version"] != want {
			t.Fatalf("int64 precision lost: %s", w.Body.String())
		}
	}
	if rows[1].(map[string]any)["workerAssignment"] != nil {
		t.Fatal("managed actor synthesized native worker assignment")
	}
	empty := httptest.NewRecorder()
	writeManagedList(empty, []*ateapi.Actor(nil), nil)
	if empty.Body.String() != "{\"results\":[]}\n" {
		t.Fatalf("empty envelope changed: %s", empty.Body.String())
	}
}
