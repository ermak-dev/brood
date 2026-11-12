package server

// BUG: /api/commands/* wrote the raw err.Error() of every error into detail,
// a deliberate refusal and an internal store error alike, so bro could show
// neither and printed only the status (#276).
// SPEC: A deliberate command refusal (command.Refusal) adds "reason", its
// public message, next to detail; an internal error has no reason, and a
// refusal that replaces or accompanies an internal error never carries that
// error's text in reason. Status, detail and X-Brood-Command-Outcome stay as
// before; a reason longer than command.MaxReasonBytes or with a control
// character is omitted.
// IMPLEMENTATION: writeCommandError in handleCommand; refusals of the command
// layer are built with command.Refuse and command.Refusef.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
)

// commandError — ответ /api/commands/* с отказом.
type commandError struct {
	status         int
	outcome        string
	detail, reason string
	hasReason      bool
	fields         map[string]any
}

func postCommand(t *testing.T, ts *substrateServer, client *http.Client, tool, swarm string, arguments map[string]any) commandError {
	t.Helper()
	r, b, raw := ts.do(client, "POST", "/api/commands/"+tool, ts.csrf(client), map[string]any{"swarm_id": swarm, "arguments": arguments})
	if r.StatusCode < 400 {
		t.Fatalf("%s succeeded: %d %s", tool, r.StatusCode, raw)
	}
	got := commandError{status: r.StatusCode, outcome: r.Header.Get("X-Brood-Command-Outcome"), fields: b}
	got.detail, _ = b["detail"].(string)
	got.reason, got.hasReason = b["reason"].(string)
	if _, present := b["reason"]; present && !got.hasReason {
		t.Fatalf("%s reason is not a string: %s", tool, raw)
	}
	return got
}

// TestCommandRefusalCarriesReason: отказ update актору, чей runtime старше
// #133 (#276, worker2), и другие сознательные отказы называют причину в
// reason; detail и статус прежние.
func TestCommandRefusalCarriesReason(t *testing.T) {
	ts, alice, master, worker, _, _ := updateSetup(t)
	guestReleaseHub(ts, func(string) (relay.SessionResponse, error) {
		return relay.SessionResponse{Text: "unsupported session operation", Outcome: "unsupported"}, nil
	})
	for name, tc := range map[string]struct {
		tool      string
		swarm     string
		arguments map[string]any
		status    int
		reason    string
		// labeled — detail начинается с класса, как у fmt.Errorf("%w: ...").
		labeled string
	}{
		"update before #133": {tool: "update", arguments: map[string]any{"name": worker.Name}, status: http.StatusConflict, labeled: "conflict: ",
			reason: `update refused: actor "worker" did not report its current guest release (its guest runtime predates guest_release), and update does not restart a container whose release it cannot check; a guest runtime older than #133 cannot report it: a one-time stop and start installs one that can, but only in a container created from a template with the bootstrap of #184; a container from an earlier template must not be stopped or restarted after a deploy that changed the guest artifacts (docs/PROXMOX.md)`},
		"delete without confirm": {tool: "delete", arguments: map[string]any{"name": worker.Name}, status: http.StatusConflict,
			reason: "delete destroys the actor disk; confirm=true is required"},
		"reset without confirm": {tool: "reset", arguments: map[string]any{"name": worker.Name}, status: http.StatusConflict,
			reason: "reset destroys the actor container and workspace; confirm=true is required"},
		"absent actor": {tool: "stop", arguments: map[string]any{"name": "ghost"}, status: http.StatusNotFound, labeled: "not found: ",
			reason: "managed actor is not available in this swarm"},
		"no swarm scope": {tool: "stop", swarm: "-", arguments: map[string]any{"name": worker.Name}, status: http.StatusConflict,
			reason: "a swarm scope is required"},
		"unknown swarm": {tool: "stop", swarm: "no-such-swarm", arguments: map[string]any{"name": worker.Name}, status: http.StatusNotFound,
			reason: "swarm not found"},
	} {
		t.Run(name, func(t *testing.T) {
			swarm := tc.swarm
			switch swarm {
			case "":
				swarm = master.SwarmID
			case "-":
				swarm = ""
			}
			got := postCommand(t, ts, alice, tc.tool, swarm, tc.arguments)
			if got.status != tc.status || got.detail != tc.labeled+tc.reason {
				t.Fatalf("status %d detail %q, want %d %q", got.status, got.detail, tc.status, tc.labeled+tc.reason)
			}
			if got.reason != tc.reason {
				t.Fatalf("reason %q (present %v), want %q", got.reason, got.hasReason, tc.reason)
			}
			if got.outcome != "" {
				t.Fatalf("refusal carries outcome header %q", got.outcome)
			}
		})
	}
}

// TestCommandInternalErrorHasNoReason: сбой хранилища — не отказ: detail
// прежний, причины нет.
func TestCommandInternalErrorHasNoReason(t *testing.T) {
	ts, alice, master, worker, _, _ := updateSetup(t)
	execDB(t, ts, `ALTER TABLE coordination_claims RENAME TO coordination_claims_276`)
	got := postCommand(t, ts, alice, "dispatch", master.SwarmID, map[string]any{"action": "show", "to": worker.Name, "request_id": "show-276"})
	// Предусловие фикстуры: ответ — сырая ошибка SQLite, как до #276.
	if got.status != http.StatusConflict || !strings.Contains(got.detail, "no such table") {
		t.Fatalf("fixture did not reach a store failure: %+v", got)
	}
	if got.hasReason {
		t.Fatalf("internal error carries reason %q", got.reason)
	}
}

// TestCommandRefusalKeepsInternalErrorOutOfReason: отказ, заменивший сбой
// хранилища, называет фиксированную причину, а не текст сбоя.
func TestCommandRefusalKeepsInternalErrorOutOfReason(t *testing.T) {
	ts, alice, master, worker, _, _ := updateSetup(t)
	execDB(t, ts, `ALTER TABLE managed_pools RENAME TO managed_pools_276`)
	got := postCommand(t, ts, alice, "update", master.SwarmID, map[string]any{"name": worker.Name})
	want := "managed lifecycle refused; reconcile the current allocation and verify namespace and provider availability"
	if got.status != http.StatusConflict || got.detail != "conflict: "+want || got.reason != want {
		t.Fatalf("refusal over a store failure: %+v", got)
	}
	for _, internal := range []string{"no such table", "managed_pools", "sqlite", "SQL"} {
		if strings.Contains(got.reason, internal) {
			t.Fatalf("reason carries the store failure: %q", got.reason)
		}
	}
}

// TestCommandUnconfirmedOutcomeKeepsHeaderWithoutReason: неизвестный исход
// — не отказ: заголовок прежний, причины нет.
func TestCommandUnconfirmedOutcomeKeepsHeaderWithoutReason(t *testing.T) {
	ts, alice, master, _ := masterSetup(t)
	ts.srv.managed.hub = commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		return relay.SessionResponse{Text: "synthetic-guest-text", Outcome: "unknown"}, nil
	}}
	got := postCommand(t, ts, alice, "slash", master.SwarmID, map[string]any{"name": "master", "command": "/clear"})
	if got.status != http.StatusConflict || got.outcome != "unconfirmed" {
		t.Fatalf("unknown slash: %+v", got)
	}
	if got.hasReason {
		t.Fatalf("unknown outcome carries reason %q", got.reason)
	}
}

// legacyCommandError — ответ на ошибку команды до #276 (handleCommand).
func legacyCommandError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, app.ErrNotAllowed) || errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrConflict) {
		apiError(w, r, err)
	} else {
		writeDetail(w, 409, err.Error())
	}
}

// TestCommandRefusalStatusAndDetailUnchanged: reason только добавляется;
// статус, detail и заголовки — те же, что до #276.
func TestCommandRefusalStatusAndDetailUnchanged(t *testing.T) {
	internal := errors.New("sqlite: synthetic-secret")
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"conflict":           {command.Refusef(app.ErrConflict, "update refused: actor %q is stopped", "w"), `update refused: actor "w" is stopped`},
		"not allowed":        {command.Refuse(app.ErrNotAllowed, `actor command not authorized: "w" is another master`), `actor command not authorized: "w" is another master`},
		"not found":          {command.Refusef(app.ErrNotFound, "managed actor is not available in this swarm"), "managed actor is not available in this swarm"},
		"classless":          {command.Refuse(nil, "dispatch requires to and request_id"), "dispatch requires to and request_id"},
		"other class":        {command.Refuse(app.ErrInvalidWorkload, "refused"), "refused"},
		"refusal with cause": {fmt.Errorf("%w: %v", command.Refuse(nil, "invalid coordination arguments"), internal), "invalid coordination arguments"},
		"internal":           {err: internal},
		"classed internal":   {err: fmt.Errorf("%w: %w", app.ErrConflict, internal)},
		"oversize reason":    {err: command.Refuse(app.ErrConflict, strings.Repeat("r", command.MaxReasonBytes+1))},
		"control reason":     {err: command.Refuse(nil, "refused\x1b[2J")},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/commands/update", nil)
			before, after := httptest.NewRecorder(), httptest.NewRecorder()
			legacyCommandError(before, r, tc.err)
			writeCommandError(after, r, tc.err)
			if after.Code != before.Code {
				t.Fatalf("status %d, before #276 %d", after.Code, before.Code)
			}
			var old, got map[string]any
			if err := json.Unmarshal(before.Body.Bytes(), &old); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(after.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["detail"] != old["detail"] {
				t.Fatalf("detail %q, before #276 %q", got["detail"], old["detail"])
			}
			reason, present := got["reason"]
			if tc.reason == "" && present || tc.reason != "" && reason != tc.reason {
				t.Fatalf("reason %v (present %v), want %q", reason, present, tc.reason)
			}
			if strings.Contains(fmt.Sprint(reason), "synthetic-secret") {
				t.Fatalf("reason carries internal text: %v", reason)
			}
			delete(got, "reason")
			if len(got) != len(old) {
				t.Fatalf("body %v, before #276 %v", got, old)
			}
			if fmt.Sprint(after.Header()) != fmt.Sprint(before.Header()) {
				t.Fatalf("headers %v, before #276 %v", after.Header(), before.Header())
			}
		})
	}
}
