//go:build linux

package guest

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// BUG: The pinned Claude SDK uses limit and beta query parameters.
// HYPOTHESIS: The loopback gateway denies standard CLI requests before relay.
func TestGuestModelSDKQueries(t *testing.T) {
	queries := make(chan string, 8)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery
		io.WriteString(w, `{"data":[]}`)
	}))
	defer backend.Close()
	handler, err := NewModelGateway(relay.ClientConfig{URL: "wss" + strings.TrimPrefix(backend.URL, "https") + "/api/proxmox/guest/actor/1", ActorUID: "actor", Generation: 1, Token: strings.Repeat("x", 43), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, query string }{
		{"GET", "/v1/models?limit=1000", "limit=1000"},
		{"GET", "/v1/models?limit=1", "limit=1"},
		{"POST", "/v1/messages?beta=true", "beta=true"},
		{"POST", "/v1/messages/count_tokens?beta=true", "beta=true"},
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Host = modelAddress
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Errorf("%s: HTTP %d", tc.path, response.Code)
			continue
		}
		select {
		case q := <-queries:
			if q != tc.query {
				t.Errorf("query lost: %q", q)
			}
		default:
			t.Fatal("backend was not called")
		}
	}
	for _, path := range []string{"/v1/models?limit=0", "/v1/models?limit=1001", "/v1/models?limit=01", "/v1/models?limit=1000&limit=1", "/v1/models?limit=1000&url=evil", "/v1/models?beta=true", "/v1/models?", "/v1/models?%6cimit=1000", "/v1/messages?limit=1000", "/v1/messages?beta=false", "/v1/messages?beta=true&x=1"} {
		method := "GET"
		if strings.Contains(path, "messages") {
			method = "POST"
		}
		request := httptest.NewRequest(method, path, nil)
		request.Host = modelAddress
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Errorf("unsafe query %s: HTTP %d", path, response.Code)
		}
	}
	if len(queries) != 0 {
		t.Fatal("unsafe query reached relay")
	}
}
