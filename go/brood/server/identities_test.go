// FEATURE: A server-owned catalog for native Substrate policy choices.
// SPEC: Local login names are not fabricated as native identities; only admins can read it.
package server

import (
	"encoding/json"
	"strings"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestNativeIdentityCatalog(t *testing.T) {
	fake := newFakeAteapi()
	var policy ateapi.AccessPolicy
	if err := protojson.Unmarshal([]byte(`{"metadata":{"name":"default"},"bindings":[{"role":"viewer","members":["user:system:serviceaccount:team:reader","user:system:serviceaccount:team:reader"]}]}`), &policy); err != nil {
		t.Fatal(err)
	}
	fake.policies[""] = &policy
	ts := newSubstrateServer(t, fake)
	admin, alice, _, _ := actorsSetup(t, ts.testServer)
	r, _, raw := ts.do(admin, "GET", "/api/identities", "", nil)
	expectStatus(t, "identities", r, nil, 200)
	var identities []map[string]string
	if err := json.Unmarshal([]byte(raw), &identities); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, identity := range identities {
		if seen[identity["id"]] {
			t.Fatal("duplicate identity")
		}
		seen[identity["id"]] = true
		if identity["id"] == "user:alice" || identity["id"] == "user:bob" || identity["id"] == "user:root" {
			t.Fatal("invented native identity from a local username")
		}
	}
	for _, id := range []string{"user:system:serviceaccount:brood:django", "user:system:serviceaccount:ate-system:ate-client", "user:system:serviceaccount:team:reader"} {
		if !seen[id] {
			t.Fatalf("missing known identity %s: %s", id, raw)
		}
	}
	if strings.Contains(raw, "test-token") || strings.Contains(raw, "password") {
		t.Fatal("credential leak")
	}
	r, data, _ := ts.do(alice, "GET", "/api/identities", "", nil)
	expectStatus(t, "ordinary user", r, data, 403)
	r, data, _ = ts.do(newClient(t), "GET", "/api/identities", "", nil)
	expectStatus(t, "anonymous", r, data, 401)
}
