package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"brood.dev/go/brood/auth"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func commandRequestBody(w http.ResponseWriter, r *http.Request) (string, json.RawMessage, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, relay.MaxMessageBytes))
	if err != nil {
		writeDetail(w, 400, "command request exceeds size limit")
		return "", nil, false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		writeDetail(w, 400, "invalid command request")
		return "", nil, false
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, e := d.Token()
		key, ok := token.(string)
		if e != nil || !ok || fields[key] != nil || (key != "swarm_id" && key != "arguments") {
			writeDetail(w, 400, "invalid or duplicate command field")
			return "", nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			writeDetail(w, 400, "invalid command request")
			return "", nil, false
		}
		fields[key] = value
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		writeDetail(w, 400, "invalid command request")
		return "", nil, false
	}
	var scope string
	if raw, ok := fields["swarm_id"]; ok && (bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &scope) != nil) {
		writeDetail(w, 400, "invalid swarm scope")
		return "", nil, false
	}
	args := fields["arguments"]
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return scope, args, true
}

func (s *Server) executeOperatorCommand(ctx context.Context, userID, swarmID, caller string, inv command.Invocation) (command.Result, error) {
	switch inv.Name {
	case "models", "profile", "project_add", "project_limit", "node", "node_drain", "node_up", "server_diagnose":
		return s.executeAdminCommand(ctx, userID, swarmID, inv)
	case "add", "delete", "stop", "start", "reset", "update":
		if swarmID == "" {
			return command.Result{}, command.Refuse(nil, "a swarm scope is required")
		}
		return s.executeActorCommand(ctx, userID, swarmID, inv)
	case "tail", "slash", "session_history", "session_terminal", "stat", "restart", "wipe", "recycle":
		if swarmID == "" {
			return command.Result{}, command.Refuse(nil, "a swarm scope is required")
		}
		return s.executeOperatorSessionCommand(ctx, userID, swarmID, caller, inv)
	case "agents":
		if swarmID == "" {
			return command.Result{}, command.Refuse(nil, "a swarm scope is required")
		}
		actors, err := s.store.ListManagedActors(ctx, swarmID)
		if err != nil {
			return command.Result{}, errors.New("actor inventory unavailable")
		}
		rows := []map[string]any{}
		for _, actor := range actors {
			if actor.Phase != "deleted" {
				rows = append(rows, map[string]any{"name": actor.Name, "role": actor.Role, "state": actor.Phase, "provider": "proxmox", "generation": actor.Incarnation})
			}
		}
		data, _ := json.Marshal(map[string]any{"scope": "managed actors in this swarm", "actors": rows})
		return command.Result{Text: "Managed actor roster; running does not mean idle or a clean workspace.", Data: data, Outcome: "completed"}, nil
	case "dispatch", "landing":
		return s.executeOperatorCoordinationCommand(ctx, userID, swarmID, inv)
	case "send":
		return s.sendOperatorCommand(ctx, userID, swarmID, inv)
	}
	return command.Result{}, command.Refuse(nil, "command is not supported in this execution mode")
}

// writeCommandError отвечает на ошибку команды. Detail — как прежде, текст
// ошибки; его клиент не показывает: у внутренней ошибки он может нести
// подробности хранилища или провайдера. Сознательный отказ (command.Refusal)
// добавляет reason — публичную причину в пределах command.PublicReason,
// которую клиент показывает (#276). Статус от reason не зависит: класс
// отказа отображается так же, как apiError и ветка 409 отображали его раньше.
func writeCommandError(w http.ResponseWriter, r *http.Request, err error) {
	reason := command.PublicReason(err)
	if reason == "" {
		if errors.Is(err, app.ErrNotAllowed) || errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrConflict) {
			apiError(w, r, err)
		} else {
			writeDetail(w, 409, err.Error())
		}
		return
	}
	status := http.StatusConflict
	switch {
	case errors.Is(err, app.ErrNotAllowed):
		status = http.StatusForbidden
	case errors.Is(err, app.ErrNotFound):
		status = http.StatusNotFound
	}
	writeJSON(w, status, struct {
		Detail string `json:"detail"`
		Reason string `json:"reason"`
	}{err.Error(), reason})
}

// commandPreflight — проверки маршрута команды каталога до исполнения,
// общие для POST /api/commands/{tool} и команд Telegram (#284): у команды в
// рое — право на рой (читать для agents и profile, иначе управлять), живой
// рой и его namespace; у команды не для гостей — администратор. refusal —
// сознательный отказ (writeCommandError), failure — сбой проверки (apiError).
func (s *Server) commandPreflight(ctx context.Context, user store.User, scope string, inv command.Invocation) (refusal, failure error) {
	spec, _ := command.Lookup(inv.Name)
	if scope != "" {
		action := app.ActionManageSwarm
		if spec.ReadOnly && (inv.Name == "agents" || inv.Name == "profile") {
			action = app.ActionViewSwarm
		}
		allowed, err := s.app.Can(user.ID, action, app.SwarmRes(scope))
		if err != nil {
			return nil, err
		}
		if !allowed {
			return command.Refuse(app.ErrNotFound, "swarm not found"), nil
		}
		sw, found, err := s.gateway.SwarmByID(scope)
		if err != nil {
			return nil, err
		}
		if !found || sw.State != app.StateActive {
			return command.Refuse(app.ErrConflict, "swarm is not active"), nil
		}
		if err := s.checkSwarmNamespace(ctx, sw); err != nil {
			return nil, err
		}
	}
	if !spec.Guest && !user.IsAdmin {
		return command.Refuse(app.ErrNotAllowed, "administrator access required"), nil
	}
	return nil, nil
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	scope, args, ok := commandRequestBody(w, r)
	if !ok {
		return
	}
	inv, err := command.ParseTool(r.PathValue("tool"), args)
	if err != nil {
		writeDetail(w, 400, err.Error())
		return
	}
	refusal, failure := s.commandPreflight(r.Context(), user, scope, inv)
	if failure != nil {
		apiError(w, r, failure)
		return
	}
	if refusal != nil {
		writeCommandError(w, r, refusal)
		return
	}
	caller := "user:" + user.ID
	if cookie, err := r.Cookie(auth.CookieSession); err == nil {
		caller += fmt.Sprintf(":%x", sha256.Sum256([]byte(cookie.Value)))
	}
	commandContext := context.WithValue(r.Context(), commandSessionAuthorityKey{}, func(ctx context.Context) bool {
		cookie, err := r.Cookie(auth.CookieSession)
		if err != nil {
			return false
		}
		id, _, err := auth.NewSessions(s.store, 0).Load(ctx, cookie.Value, time.Now())
		return err == nil && id == user.ID
	})
	result, err := s.executeOperatorCommand(commandContext, user.ID, scope, caller, inv)
	if err != nil {
		if result.Outcome == "unconfirmed" || result.Outcome == "unknown" {
			w.Header().Set("X-Brood-Command-Outcome", "unconfirmed")
		}
		writeCommandError(w, r, err)
		return
	}
	if !s.validProxySession(r) {
		writeDetail(w, 403, "command session authorization changed")
		return
	}
	if result.Outcome == "ok" || result.Outcome == "complete" {
		result.Outcome = "completed"
	}
	status := http.StatusOK
	if result.Outcome == "accepted" {
		status = http.StatusAccepted
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > relay.MaxMessageBytes {
		writeDetail(w, 502, "command result exceeds size limit")
		return
	}
	writeJSON(w, status, result)
}
