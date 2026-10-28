// FEATURE: Native Substrate policies are an administrator-only infrastructure resource.
// SPEC: Ownership of a swarm must not bypass the native policy restriction.
package server

import "testing"

func TestNativeSwarmPolicyAdminOnly(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, owner, other, swarm := actorsSetup(t, ts.testServer)
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			for _, client := range []struct {
				name      string
				ownsSwarm bool
			}{{"owner", true}, {"other", false}} {
				actor := other
				if client.ownsSwarm {
					actor = owner
				}
				r, data, _ := ts.do(actor, method, "/api/swarms/"+swarm+"/policy", ts.csrf(actor), map[string]any{"bindings": []any{}})
				expectStatus(t, client.name+" "+method, r, data, 403)
			}
		})
	}
	r, data, _ := ts.do(newClient(t), "GET", "/api/swarms/"+swarm+"/policy", "", nil)
	expectStatus(t, "anonymous native policy", r, data, 401)
}
