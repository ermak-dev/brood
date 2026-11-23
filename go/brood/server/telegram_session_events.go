package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/store"
)

const guestSessionEventPrefix = "/api/proxmox/session-events/"

func publicGuestSessionEventPath(path string) bool {
	return strings.HasPrefix(path, guestSessionEventPrefix)
}

type guestSessionEvent struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	State     string `json:"state"`
	ErrorCode string `json:"error_code"`
}

func (s *Server) serveGuestSessionEvent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeDetail(w, 405, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	g, _, ok := s.authorizeGuestRuntime(ctx, w, r, guestSessionEventPrefix, noGuestRuntimeRest)
	if !ok {
		return
	}
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer control.SetReadDeadline(time.Time{})
	var event guestSessionEvent
	if !readSessionEvent(w, r, &event) {
		writeDetail(w, 400, "invalid agent session event; no notification was sent")
		return
	}
	if !s.guestRuntimeCurrent(ctx, g) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	s.telegramActivity.update(g, event)
	if event.State == "error" {
		_, destination, linked := s.liveTelegramLink(ctx, g.swarm.OwnerID)
		first, err := s.store.MarkTelegramSessionError(ctx, g.uid, g.generation, event.SessionID, event.TurnID)
		if err != nil {
			writeDetail(w, 503, "could not record the agent session failure; no notification was sent")
			return
		}
		if first && linked {
			ctx = context.WithValue(ctx, telegramOutboundBindingKey{}, destination)
			s.notifyTelegramSessionError(ctx, g, event.ErrorCode)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func readSessionEvent(w http.ResponseWriter, r *http.Request, event *guestSessionEvent) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(event) != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	if !projectSecretActorUID.MatchString(event.SessionID) || !projectSecretActorUID.MatchString(event.TurnID) {
		return false
	}
	switch event.State {
	case "working", "idle":
		return event.ErrorCode == ""
	case "error":
		return event.ErrorCode == "" || len(event.ErrorCode) <= 32 && projectSecretActorUID.MatchString(event.ErrorCode)
	default:
		return false
	}
}

func (s *Server) notifyTelegramSessionError(ctx context.Context, g guestRuntime, code string) {
	project, err := s.store.GetProject(ctx, g.swarm.ProjectID)
	if err != nil {
		return
	}
	sw, err := s.store.GetSwarm(ctx, g.swarm.ID)
	if err != nil || sw.OwnerID != g.swarm.OwnerID || !s.guestRuntimeCurrent(ctx, g) {
		return
	}
	body := fmt.Sprintf("Claude Code at %s/%s/%s: %s", project.Name, sw.Name, g.actor.Name, sessionErrorText(code))
	_ = s.telegramChannel.toOwner(ctx, g.actor, body)
}

func sessionErrorText(code string) string {
	switch code {
	case "invalid_request", "invalid_request_error", "400":
		return "the model rejected the request (HTTP 400). Check the gateway log before retrying."
	case "authentication_failed", "authentication_error", "cloud_credential_error", "401":
		return "model authentication failed (HTTP 401). Check the model gateway credentials."
	case "permission_denied", "permission_error", "403":
		return "the model backend denied access (HTTP 403). Check the gateway permissions."
	case "rate_limit", "rate_limit_error", "429":
		return "the model backend rate limit was reached (HTTP 429). Retry later."
	case "billing_error", "account_on_hold":
		return "the model backend refused the request for account or billing reasons. Check the provider account."
	case "oauth_org_not_allowed":
		return "the model backend did not allow this organization to use the session. Check the gateway account."
	case "model_not_found":
		return "the configured model was not found. Check the role's model and the gateway catalog."
	case "server_error", "api_error", "overloaded_error", "500", "529":
		return "the model backend could not finish the request. Check the gateway or retry later."
	case "max_output_tokens":
		return "the response reached its output limit. Ask the agent to continue in a shorter turn."
	case "launch", "launch_failed", "startup_failed":
		return "the session could not start. Check the agent terminal before restarting."
	case "process_exit", "process_exited", "crash", "session_exit":
		return "the session ended unexpectedly. Check the agent terminal before restarting."
	default:
		return "the turn failed. Check the agent terminal and model gateway before retrying."
	}
}

func (s *Server) liveTelegramLink(ctx context.Context, userID string) (store.TelegramLink, telegramChat, bool) {
	link, err := s.store.GetTelegramLink(ctx, userID)
	if err != nil || link.State != "linked" || link.ChatID == 0 {
		return link, telegramChat{}, false
	}
	session, err := s.store.TelegramChatSession(ctx, userID, link.BotUsername, link.ChatID, link.TelegramUserID)
	if err != nil || session == "" {
		return link, telegramChat{}, false
	}
	return link, telegramChat{userID: userID, botUsername: link.BotUsername, session: session, chatID: link.ChatID}, true
}
