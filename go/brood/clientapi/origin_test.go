package clientapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMutationsSendTheConfiguredServerOrigin(t *testing.T) {
	var origin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin || r.Header.Get("X-CSRFToken") != "csrf-value" {
			http.Error(w, "invalid origin or CSRF", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	origin = server.URL
	client, err := New(origin)
	if err != nil {
		t.Fatal(err)
	}
	client.Restore(StoredSession{Session: "session-value", CSRF: "csrf-value"})
	if err := client.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
}
