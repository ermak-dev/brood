package clientapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnconfirmedHeaderIsScopedAndKeepsAuthErrors(t *testing.T) {
	for _, tc := range []struct {
		path    string
		status  int
		want    error
		warning bool
	}{
		{"/api/auth/login", 401, ErrInvalidCredentials, false},
		{"/api/projects/example/secrets/vars", 409, nil, false},
		{"/api/commands/restart", 401, ErrSessionExpired, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Brood-Command-Outcome", "unconfirmed")
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"detail":"synthetic-cookie"}`)
			}))
			defer server.Close()
			c, _ := New(server.URL)
			err := c.call(context.Background(), "POST", tc.path, []byte(`{}`), nil)
			if err == nil || strings.Contains(err.Error(), "synthetic-cookie") {
				t.Fatal("unsafe or missing failure")
			}
			if strings.Contains(err.Error(), "outcome is unconfirmed") != tc.warning {
				t.Fatal("outcome escaped command transport scope")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatal("typed authentication failure was lost")
			}
		})
	}
}
