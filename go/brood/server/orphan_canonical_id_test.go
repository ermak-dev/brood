package server

import (
	"context"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
)

// FEATURE: Recovery keeps the canonical identifier used by existing URLs (#68).
// SPEC: Case aliases map to the same native name but are not accepted as a new
// local ID: ULIDs are uppercase and UUIDs use lowercase hexadecimal.
func TestOrphanRecoveryRejectsIdentifierCaseAliases(t *testing.T) {
	for _, id := range []string{strings.ToLower(recoveryTestID), "01234567-89AB-CDEF-0123-456789ABCDEF"} {
		for _, method := range []string{"GET", "POST"} {
			t.Run(method+id, func(t *testing.T) {
				fake := newFakeAteapi()
				ts := newSubstrateServer(t, fake)
				admin, _, _, knownID := actorsSetup(t, ts.testServer)
				known, err := ts.srv.store.GetSwarm(context.Background(), knownID)
				if err != nil {
					t.Fatal(err)
				}
				addRecoveryNamespace(fake, app.AtespaceName(id), "expected-uid")
				before := recoveryReads(fake)
				path := "/api/admin/orphan-swarms/" + id + "?atespace_uid=expected-uid"
				var body any
				if method == "POST" {
					path = "/api/admin/orphan-swarms/recover"
					body = map[string]string{"project_id": known.ProjectID, "swarm_id": id, "atespace_uid": "expected-uid"}
				}
				r, b, _ := ts.do(admin, method, path, ts.csrf(admin), body)
				expectStatus(t, "case alias", r, b, 400)
				if recoveryReads(fake) != before {
					t.Fatal("case alias reached native API")
				}
			})
		}
	}
}
