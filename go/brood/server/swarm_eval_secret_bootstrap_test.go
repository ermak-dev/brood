package server

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

// BUG: Secret-export 403 останавливает обязательный guest bootstrap в eval.
// HYPOTHESIS: Нужна authenticated пустая projection, не игнорирование 403:
// реальный guest decoder обязан принять её без чтения project secret store.
func TestEvalRuntimeGuestFetchesOnlyEmptySecretProjection(t *testing.T) {
	f := projectCredentialGateway(t)
	runtimeEvalMarker(t, f.ts, f.swarm)
	if _, err := f.ts.srv.catalog.db.Exec(`DROP TABLE project_secret_vars`); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewTLSServer(http.HandlerFunc(f.ts.srv.serveGuestProjectSecrets))
	defer endpoint.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.Certificate().Raw})
	cfg := relay.ClientConfig{URL: "wss" + strings.TrimPrefix(endpoint.URL, "https"), ActorUID: f.actor.UID, Generation: f.actor.Incarnation, Token: f.token, CAPEM: ca}
	for range 2 {
		bundle, err := guest.FetchProjectSecrets(context.Background(), cfg)
		if err != nil {
			t.Fatalf("eval guest cannot pass mandatory authenticated secret bootstrap: %v", err)
		}
		if err := projectsecret.ValidateBundle(bundle); err != nil || bundle.Revision != 0 || len(bundle.Files) != 0 || len(bundle.Vars) != 0 {
			t.Fatalf("eval guest received project data instead of an isolated empty projection: %+v %v", bundle, err)
		}
	}
	cfg.Token = strings.Repeat("x", 43)
	if _, err := guest.FetchProjectSecrets(context.Background(), cfg); err == nil {
		t.Fatal("secret-free eval bootstrap bypassed guest authentication")
	}
}
