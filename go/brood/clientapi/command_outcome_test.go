package clientapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

// BUG: Redacting untrusted error bodies also hides warnings about unknown effects.
// SPEC: Only fixed command outcome metadata becomes a static warning; body/header contents never become diagnostics.
func TestCommandErrorPreservesSafeUnconfirmedWarning(t *testing.T) {
	for _, header := range []string{"unconfirmed", "unknown", "unexpected-secret-header"} {
		t.Run(header, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Brood-Command-Outcome", header)
				w.WriteHeader(409)
				io.WriteString(w, `{"detail":"synthetic-password synthetic-cookie"}`)
			}))
			defer server.Close()
			c, _ := New(server.URL)
			_, err := (CommandExecutor{Client: c, SwarmID: "scope"}).Execute(context.Background(), command.Invocation{Name: "restart", Arguments: json.RawMessage(`{"name":"master","confirm":true}`)})
			if err == nil {
				t.Fatal("error status reported success")
			}
			message := err.Error()
			if strings.Contains(message, "synthetic-") || strings.Contains(message, "unexpected-secret-header") {
				t.Fatal("untrusted error data disclosed")
			}
			want := header == "unconfirmed" || header == "unknown"
			if strings.Contains(message, "outcome is unconfirmed") != want {
				t.Fatalf("warning presence differs: %v", err)
			}
			if want && !strings.Contains(message, "check before retrying") {
				t.Fatal("retry warning missing")
			}
		})
	}
}
