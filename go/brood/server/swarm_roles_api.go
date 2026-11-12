package server

// Привязка ролей роя к акторам (#235): представление ролей, привязка и
// снятие. Читает любая роль роя; меняет управляющий роем, как запуск
// (#211): сервер пускает видящего рой, сессия, право управлять, исполнение
// и актор перепроверяются хранилищем в транзакции записи. Клиент видит
// только фиксированные публичные причины отказа.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"brood.dev/go/brood/store"
)

// maxSwarmRoleBody ограничивает тело привязки: UID и инкарнация.
const maxSwarmRoleBody = 4096

// swarmRoleInvalidBody — причина отказа неразборчивому телу привязки.
const swarmRoleInvalidBody = "invalid swarm role binding: expected {actorUid, actorIncarnation?}"

type swarmRoleGraphJSON struct {
	Pinned   bool   `json:"pinned"`
	Name     string `json:"name"`
	Version  int64  `json:"version"`
	Revision string `json:"revision"`
}

// swarmRoleJSON — роль графа или привязки. State — вердикт запуска по
// актору: ready, unbound, stale, inactive, foreign или missing.
type swarmRoleJSON struct {
	Role             string `json:"role"`
	InGraph          bool   `json:"inGraph"`
	State            string `json:"state"`
	ActorUID         string `json:"actorUid,omitempty"`
	ActorName        string `json:"actorName,omitempty"`
	ActorIncarnation int64  `json:"actorIncarnation,omitempty"`
}

type swarmRolesJSON struct {
	SwarmID   string              `json:"swarmId"`
	Execution string              `json:"execution"`
	Graph     *swarmRoleGraphJSON `json:"graph"`
	Roles     []swarmRoleJSON     `json:"roles"`
}

func (s *Server) mountSwarmRolesAPI() {
	s.mux.HandleFunc("GET /api/swarms/{id}/roles", s.handleSwarmRoles)
	s.mux.HandleFunc("PUT /api/swarms/{id}/roles/{role}", s.handleSwarmRoleBind)
	s.mux.HandleFunc("DELETE /api/swarms/{id}/roles/{role}", s.handleSwarmRoleUnbind)
}

// handleSwarmRoles: GET /api/swarms/{id}/roles[?definition=NAME]. Граф —
// снимок роя; до первого запуска — названное определение каталога.
func (s *Server) handleSwarmRoles(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	view, err := s.store.ListSwarmRoles(r.Context(), r.PathValue("id"), r.URL.Query().Get("definition"))
	if err != nil {
		swarmRoleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, swarmRolesView(view))
}

// handleSwarmRoleBind: PUT /api/swarms/{id}/roles/{role} {actorUid, actorIncarnation?}.
func (s *Server) handleSwarmRoleBind(w http.ResponseWriter, r *http.Request) {
	user, ok := s.visibleSwarmUser(w, r)
	if !ok {
		return
	}
	in, ok := readSwarmRoleBinding(w, r)
	if !ok {
		return
	}
	in.SwarmID, in.UserID, in.Role = r.PathValue("id"), user.ID, r.PathValue("role")
	view, err := s.store.BindSwarmRoleAs(r.Context(), in, func(ctx context.Context) bool {
		return secretRequestSession(ctx, s, r, user.ID)
	})
	if err != nil {
		swarmRoleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, swarmRolesView(view))
}

// handleSwarmRoleUnbind: DELETE /api/swarms/{id}/roles/{role}.
func (s *Server) handleSwarmRoleUnbind(w http.ResponseWriter, r *http.Request) {
	user, ok := s.visibleSwarmUser(w, r)
	if !ok {
		return
	}
	view, err := s.store.UnbindSwarmRoleAs(r.Context(), r.PathValue("id"), user.ID, r.PathValue("role"), func(ctx context.Context) bool {
		return secretRequestSession(ctx, s, r, user.ID)
	})
	if err != nil {
		swarmRoleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, swarmRolesView(view))
}

// readSwarmRoleBinding разбирает тело строго: объект с уникальными ключами,
// обязательная непустая строка actorUid и необязательное положительное
// целое actorIncarnation — инкарнация, которую видел вызывающий.
func readSwarmRoleBinding(w http.ResponseWriter, r *http.Request) (store.SwarmRoleAssignment, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSwarmRoleBody))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, swarmRoleInvalidBody)
		return store.SwarmRoleAssignment{}, false
	}
	fields, err := proxmoxObject(string(data))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, swarmRoleInvalidBody)
		return store.SwarmRoleAssignment{}, false
	}
	var in store.SwarmRoleAssignment
	for key, raw := range fields {
		valid := false
		switch key {
		case "actorUid":
			valid = syncString(raw, &in.ActorUID)
		case "actorIncarnation":
			valid = positiveInteger(raw, &in.ActorIncarnation)
		}
		if !valid {
			writeDetail(w, http.StatusBadRequest, swarmRoleInvalidBody)
			return store.SwarmRoleAssignment{}, false
		}
	}
	if in.ActorUID == "" {
		writeDetail(w, http.StatusBadRequest, swarmRoleInvalidBody)
		return store.SwarmRoleAssignment{}, false
	}
	return in, true
}

// positiveInteger принимает только JSON-целое без знака, дроби и
// экспоненты, большее нуля; null и строка не подходят.
func positiveInteger(raw []byte, target *int64) bool {
	if len(raw) == 0 || raw[0] < '1' || raw[0] > '9' {
		return false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return false
		}
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil || n <= 0 {
		return false
	}
	*target = n
	return true
}

// swarmRoleError переводит отказ хранилища в статус и фиксированную причину;
// текст ошибок хранилища наружу не идёт, внутренний сбой логируется.
func swarmRoleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrSwarmRoleInvalid):
		writeDetail(w, http.StatusBadRequest, "swarm role must be a lowercase DNS label")
	case errors.Is(err, store.ErrSwarmRequestDenied):
		writeDetail(w, http.StatusForbidden, "swarm management denied")
	case errors.Is(err, store.ErrWorkflowDefinitionMissing):
		writeDetail(w, http.StatusNotFound, "workflow definition is not in the project catalog")
	case errors.Is(err, store.ErrSwarmRoleUnbound):
		writeDetail(w, http.StatusNotFound, "swarm role is not bound")
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "swarm not found")
	case errors.Is(err, store.ErrSwarmRoleBusy):
		writeDetail(w, http.StatusConflict, "swarm execution is running or waiting; stop the swarm before changing role bindings")
	case errors.Is(err, store.ErrSwarmExecutionState):
		writeDetail(w, http.StatusConflict, "swarm is not active")
	case errors.Is(err, store.ErrSwarmRoleForeignActor):
		writeDetail(w, http.StatusConflict, "actor is not an actor of this swarm")
	// Ждущая остановка — раньше общего неживого: фаза и желание актора —
	// running, и общая причина ему противоречила бы (#275).
	case errors.Is(err, store.ErrSwarmRoleStopPending):
		writeDetail(w, http.StatusConflict, "actor is waiting to stop for a reset or update; bind it once it runs again")
	case errors.Is(err, store.ErrSwarmRoleInactive):
		writeDetail(w, http.StatusConflict, "actor is not live: it must be provisioning or running and meant to run")
	case errors.Is(err, store.ErrSwarmRoleStale):
		writeDetail(w, http.StatusConflict, "actor incarnation changed; list the swarm actors and bind again")
	case errors.Is(err, store.ErrSwarmGraphPinned):
		writeDetail(w, http.StatusConflict, "swarm graph is pinned to another workflow definition")
	case errors.Is(err, store.ErrWorkflowGraphInvalid):
		writeDetail(w, http.StatusUnprocessableEntity, "workflow graph is not executable")
	default:
		requestLogger(r.Context()).Error("swarm role binding failed", "err", err.Error())
		writeDetail(w, http.StatusInternalServerError, "internal error")
	}
}

// swarmRolesView рисует представление ролей роя.
func swarmRolesView(view store.SwarmRoles) swarmRolesJSON {
	out := swarmRolesJSON{SwarmID: view.Swarm.ID, Execution: view.Swarm.Execution, Roles: make([]swarmRoleJSON, 0, len(view.Roles))}
	if g := view.Graph; g != nil {
		out.Graph = &swarmRoleGraphJSON{Pinned: g.Pinned, Name: g.Name, Version: g.Version, Revision: g.Revision}
	}
	for _, role := range view.Roles {
		item := swarmRoleJSON{Role: role.Role, InGraph: role.InGraph, State: swarmRoleState(role), ActorName: role.ActorName}
		if b := role.Binding; b != nil {
			item.ActorUID, item.ActorIncarnation = b.ActorUID, b.ActorIncarnation
		}
		out.Roles = append(out.Roles, item)
	}
	return out
}

// swarmRoleState — публичное имя вердикта запуска по актору роли.
func swarmRoleState(role store.SwarmRoleState) string {
	switch {
	case role.Binding == nil:
		return "unbound"
	case role.Err == nil:
		return "ready"
	case errors.Is(role.Err, store.ErrSwarmRoleStale):
		return "stale"
	case errors.Is(role.Err, store.ErrSwarmRoleInactive):
		return "inactive"
	case errors.Is(role.Err, store.ErrSwarmRoleForeignActor):
		return "foreign"
	default:
		// ErrSwarmRoleUnbound при привязке: актора привязки больше нет.
		return "missing"
	}
}
