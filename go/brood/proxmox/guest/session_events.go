//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

const localSessionEventPath = "/_brood/session-events"
const sessionEventPrefix = "/api/proxmox/session-events/"

// --settings добавляет хуки только управляемому запуску, ничего не записывая
// в настройки пользователя и репозитория. Хук не участвует в решении Claude.
func sessionHookSettings() string {
	type command struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type matcher struct {
		Matcher string    `json:"matcher,omitempty"`
		Hooks   []command `json:"hooks"`
	}
	hooks := map[string][]matcher{}
	for event, match := range map[string]string{
		"SessionStart": "", "UserPromptSubmit": "", "Stop": "", "StopFailure": "", "SessionEnd": "",
		"Notification": "permission_prompt|idle_prompt", "PermissionRequest": "",
		"PreToolUse": "AskUserQuestion|ExitPlanMode", "PostToolUse": "", "PostToolUseFailure": "",
	} {
		hooks[event] = []matcher{{Matcher: match, Hooks: []command{{
			Type: "command", Command: "/opt/brood/bin/brood-guest --brood-guest-session-hook", Timeout: 1,
		}}}}
	}
	data, _ := json.Marshal(map[string]any{"hooks": hooks})
	return string(data)
}

// RunSessionHook reads Claude Code's structured hook stdin. The helper is
// silent and fail-open; it never sends prompts, tool output or error details.
func RunSessionHook(ctx context.Context, input io.Reader) {
	epoch := os.Getenv(ptysession.SessionEpochEnv)
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || !ptysession.ValidSessionID(epoch) {
		return
	}
	var in struct {
		Event        string `json:"hook_event_name"`
		SessionID    string `json:"session_id"`
		AgentID      string `json:"agent_id"`
		Notification string `json:"notification_type"`
		Error        string `json:"error"`
		Interrupt    bool   `json:"is_interrupt"`
		ToolName     string `json:"tool_name"`
		ToolUseID    string `json:"tool_use_id"`
	}
	if json.NewDecoder(io.LimitReader(input, 8<<20)).Decode(&in) != nil ||
		in.AgentID != "" || !ptysession.ValidSessionID(in.SessionID) {
		return
	}
	hook := ptysession.SessionHook{Epoch: epoch, SessionID: in.SessionID}
	switch in.Event {
	case "SessionStart":
		hook.Signal = "start"
	case "UserPromptSubmit":
		hook.Signal = "prompt"
	case "Stop", "SessionEnd":
		hook.Signal = "idle"
	case "StopFailure":
		hook.Signal, hook.ErrorCode = "failure", ptysession.SessionErrorCode(in.Error)
	case "Notification":
		switch in.Notification {
		case "permission_prompt":
			hook.Signal = "pause"
		case "idle_prompt":
			hook.Signal = "idle"
		}
	case "PermissionRequest":
		hook.Signal = "pause"
	case "PreToolUse":
		if in.ToolName == "AskUserQuestion" || in.ToolName == "ExitPlanMode" {
			hook.Signal = "pause"
		}
	case "PostToolUse", "PostToolUseFailure":
		hook.Signal = "resume"
		if in.Interrupt {
			hook.Signal = "idle"
		}
	}
	if hook.Signal == "" {
		return
	}
	if len(in.ToolName) <= 256 {
		hook.ToolName = in.ToolName
	}
	if ptysession.ValidSessionID(in.ToolUseID) {
		hook.ToolUseID = in.ToolUseID
	}
	ctx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
	defer cancel()
	_, _ = ptysession.Call(ctx, ptysession.SocketPath, ptysession.Request{Action: "session_hook", Hook: &hook})
}

// sessionEventQueue keeps HTTP outside the PTY lock and preserves event order.
// Loss is preferable to blocking Claude or replaying a stale activity lease.
func sessionEventQueue(ctx context.Context) func(ptysession.SessionEvent) {
	type queuedEvent struct {
		event ptysession.SessionEvent
		at    time.Time
	}
	queue := make(chan queuedEvent, 16)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, CheckRedirect: noSessionRedirect}
	go func() {
		defer transport.CloseIdleConnections()
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case item := <-queue:
				if item.event.State == "working" && time.Since(item.at) > 8*time.Second {
					continue
				}
				timeout := sessionEventTimeout(item.event.State) + time.Second
				sendCtx, cancel := context.WithTimeout(ctx, timeout)
				postSessionEvent(sendCtx, client, "http://"+modelAddress+localSessionEventPath, "", item.event)
				cancel()
			}
		}
	}()
	return func(event ptysession.SessionEvent) {
		select {
		case queue <- queuedEvent{event: event, at: time.Now()}:
		default:
		}
	}
}

func sessionEventTimeout(state string) time.Duration {
	if state == "error" {
		return 7 * time.Second
	}
	return 3 * time.Second
}

func noSessionRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func postSessionEvent(ctx context.Context, client *http.Client, url, token string, event ptysession.SessionEvent) bool {
	body, _ := json.Marshal(event)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent
}

// sessionEventHandler only forwards the fixed schema to the authenticated
// supervisor origin; no caller headers, response bodies or redirects cross it.
func sessionEventHandler(route serverRoute) http.HandlerFunc {
	target := *route.origin
	target.Path = route.path(sessionEventPrefix)
	client := &http.Client{Transport: route.transport(7 * time.Second), CheckRedirect: noSessionRedirect}
	slot := make(chan struct{}, 1)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "invalid session event request", http.StatusBadRequest)
			return
		}
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var event ptysession.SessionEvent
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if dec.Decode(&event) != nil || dec.Decode(new(any)) != io.EOF || !validSessionEvent(event) {
			http.Error(w, "invalid session event", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sessionEventTimeout(event.State))
		defer cancel()
		if !postSessionEvent(ctx, client, target.String(), route.token, event) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func validSessionEvent(event ptysession.SessionEvent) bool {
	if !ptysession.ValidSessionID(event.SessionID) || !ptysession.ValidSessionID(event.TurnID) {
		return false
	}
	switch event.State {
	case "working", "idle":
		return event.ErrorCode == ""
	case "error":
		return event.ErrorCode != "" && event.ErrorCode == ptysession.SessionErrorCode(event.ErrorCode)
	default:
		return false
	}
}
