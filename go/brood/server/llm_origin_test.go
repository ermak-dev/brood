package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestManagementUpgradeRequiresMatchingOrigin(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	ts := newLLMTestServer(t, backend.URL)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	for _, origin := range []string{"", "null", "https://evil.invalid", ts.http.URL} {
		req, _ := http.NewRequest("GET", ts.http.URL+"/v8/management/stream", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Origin", origin)
		req.Header.Set("X-CSRFToken", ts.csrf(admin))
		response, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := http.StatusForbidden
		if origin == ts.http.URL {
			want = http.StatusOK
		}
		if response.StatusCode != want {
			t.Fatalf("origin %q: got %d, want %d", origin, response.StatusCode, want)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("untrusted origin reached management backend: %d calls", calls.Load())
	}
}
