package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG: The pinned Claude SDK adds limit and beta query parameters.
// HYPOTHESIS: Blanket query rejection blocks the real CLI before inference.
func TestManagedLLMSDKQueries(t *testing.T) {
	var queries []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		io.WriteString(w, `{"data":[]}`)
	}))
	defer backend.Close()
	ts, _, _, base, token := managedLLMSetup(t, backend.URL, "server-model-key")
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/models?limit=1000"},
		{"GET", "/v1/models?limit=1"},
		{"POST", "/v1/messages?beta=true"},
		{"POST", "/v1/messages/count_tokens?beta=true"},
	} {
		response := llmRequest(t, ts.srv.Handler(), tc.method, base+tc.path, token, strings.NewReader(""))
		if response.Code != 200 {
			t.Errorf("%s: HTTP %d", tc.path, response.Code)
		}
	}
	if strings.Join(queries, ",") != "limit=1000,limit=1,beta=true,beta=true" {
		t.Errorf("upstream query mismatch: %v", queries)
	}
	for _, path := range []string{
		"/v1/models?limit=0", "/v1/models?limit=1001", "/v1/models?limit=01",
		"/v1/models?limit=1000&limit=1", "/v1/models?limit=1000&url=evil",
		"/v1/models?beta=true", "/v1/models?", "/v1/models?%6cimit=1000",
		"/v1/messages?limit=1000", "/v1/messages?beta=false", "/v1/messages?beta=true&x=1",
	} {
		method := "GET"
		if strings.Contains(path, "messages") {
			method = "POST"
		}
		response := llmRequest(t, ts.srv.Handler(), method, base+path, token, nil)
		if response.Code != 404 {
			t.Errorf("unsafe query %s: HTTP %d", path, response.Code)
		}
	}
	if len(queries) != 4 {
		t.Fatal("unsafe query reached backend")
	}
}
