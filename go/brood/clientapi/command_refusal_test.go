package clientapi

// BUG: bro printed only "server error (status 409): Conflict" for a refused
// command, although the server named the reason (#276).
// SPEC: For a refused POST /api/commands/* the error carries the server's
// "reason" when the body is at most 4096 bytes and the reason is a string of
// at most 1024 bytes of UTF-8 without control characters. "detail" and every
// other field stay untrusted and never reach the error. The unconfirmed-outcome
// warning stays, and a body without reason (a server before #276) gives the
// error it always gave.
// IMPLEMENTATION: publicCommandRefusal in Client.callWithTimeout.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

// worker2Refusal — причина отказа update актору, чей runtime старше #133.
const worker2Refusal = `update refused: actor "worker2" did not report its current guest release (its guest runtime predates guest_release), and update does not restart a container whose release it cannot check; a guest runtime older than #133 cannot report it: a one-time stop and start installs one that can, but only in a container created from a template with the bootstrap of #184; a container from an earlier template must not be stopped or restarted after a deploy that changed the guest artifacts (docs/PROXMOX.md)`

// refusedUpdate выполняет update worker2 у сервера, который отвечает status,
// заголовком исхода header и телом body.
func refusedUpdate(t *testing.T, status int, header, body string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/commands/update" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if header != "" {
			w.Header().Set("X-Brood-Command-Outcome", header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer server.Close()
	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (CommandExecutor{Client: c, SwarmID: "s1"}).Execute(context.Background(), command.Invocation{Name: "update", Arguments: json.RawMessage(`{"name":"worker2"}`)})
	if err == nil {
		t.Fatal("refused command reported success")
	}
	return err
}

func refusalBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCommandRefusalShowsReason(t *testing.T) {
	body := refusalBody(t, map[string]any{"detail": "synthetic-detail synthetic-password", "reason": worker2Refusal, "trace": "synthetic-trace"})
	err := refusedUpdate(t, http.StatusConflict, "", body)
	want := "POST /api/commands/update: server error (status 409): Conflict: " + worker2Refusal
	if err.Error() != want {
		t.Fatalf("error %q\nwant %q", err, want)
	}
	if strings.Contains(err.Error(), "synthetic-") {
		t.Fatal("untrusted error data disclosed")
	}
}

func TestCommandRefusalKeepsUnconfirmedWarning(t *testing.T) {
	for _, header := range []string{"unconfirmed", "unknown"} {
		t.Run(header, func(t *testing.T) {
			body := refusalBody(t, map[string]any{"detail": "synthetic-detail", "reason": "workspace operation is held"})
			err := refusedUpdate(t, http.StatusConflict, header, body)
			want := "command outcome is unconfirmed; check before retrying: POST /api/commands/update: server error (status 409): Conflict: workspace operation is held"
			if err.Error() != want {
				t.Fatalf("error %q\nwant %q", err, want)
			}
		})
	}
}

// TestCommandErrorWithoutReasonIsUnchanged: тело сервера до #276 — только
// detail; ошибка та же, что прежде, при любом статусе.
func TestCommandErrorWithoutReasonIsUnchanged(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		err := refusedUpdate(t, status, "", `{"detail":"conflict: update refused: actor \"worker2\" synthetic-detail"}`)
		want := fmt.Sprintf("POST /api/commands/update: server error (status %d): %s", status, http.StatusText(status))
		if err.Error() != want {
			t.Fatalf("status %d: error %q, want %q", status, err, want)
		}
	}
}

func TestCommandRefusalRejectsUnsafeReason(t *testing.T) {
	longest := strings.Repeat("r", 1024)
	if err := refusedUpdate(t, http.StatusConflict, "", refusalBody(t, map[string]any{"reason": longest})); !strings.HasSuffix(err.Error(), ": Conflict: "+longest) {
		t.Fatalf("reason of 1024 bytes dropped: %v", err)
	}
	for name, body := range map[string]string{
		"oversize reason":   refusalBody(t, map[string]any{"reason": longest + "r"}),
		"newline":           refusalBody(t, map[string]any{"reason": "refused\nsynthetic forged line"}),
		"tab":               refusalBody(t, map[string]any{"reason": "refused\tsynthetic"}),
		"escape":            refusalBody(t, map[string]any{"reason": "refused \x1b[2Jsynthetic"}),
		"c1":                refusalBody(t, map[string]any{"reason": "refused \u009bsynthetic"}),
		"delete":            refusalBody(t, map[string]any{"reason": "refused \x7fsynthetic"}),
		"invalid utf-8":     `{"reason":"refused synthetic-` + "\xff" + `"}`,
		"empty":             refusalBody(t, map[string]any{"reason": "", "detail": "synthetic-detail"}),
		"not a string":      `{"reason":{"text":"synthetic-object"}}`,
		"number":            `{"reason":409}`,
		"malformed":         `{"reason":"synthetic-unterminated`,
		"not an object":     `["synthetic-array"]`,
		"oversize body":     refusalBody(t, map[string]any{"reason": "refused", "detail": strings.Repeat("synthetic-", 410)}),
		"duplicate trailer": `{"reason":"refused"} {"reason":"synthetic-second"}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := refusedUpdate(t, http.StatusConflict, "", body)
			if err.Error() != "POST /api/commands/update: server error (status 409): Conflict" {
				t.Fatalf("unsafe reason shown: %q", err)
			}
		})
	}
}

// TestCommandRefusalScope: reason читается только у отказа команды, а 401
// команды остаётся истёкшей сессией.
func TestCommandRefusalScope(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
		want         error
	}{
		{http.MethodPost, "/api/projects", http.StatusConflict, nil},
		{http.MethodPost, "/api/projects/example/secrets/vars", http.StatusConflict, nil},
		{http.MethodGet, "/api/commands/update", http.StatusConflict, nil},
		{http.MethodPost, "/api/commands/update", http.StatusUnauthorized, ErrSessionExpired},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"detail":"synthetic-detail","reason":"synthetic-reason"}`)
			}))
			defer server.Close()
			c, _ := New(server.URL)
			err := c.call(context.Background(), tc.method, tc.path, []byte(`{}`), nil)
			if err == nil || strings.Contains(err.Error(), "synthetic-") {
				t.Fatalf("reason escaped command refusals: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("typed failure lost: %v", err)
			}
		})
	}
}
