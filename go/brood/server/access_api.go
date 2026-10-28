package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

func (s *Server) mountAccessAPI() {
	s.mux.HandleFunc("GET /api/projects/{id}", s.handleProjectDetail)
	s.mux.HandleFunc("PUT /api/projects/{id}/capacity", s.handleShardCapacity)
	s.mux.HandleFunc("GET /api/swarms/{id}/members", s.handleSwarmMembers)
	s.mux.HandleFunc("POST /api/swarms/{id}/members", s.handleSwarmMembers)
	s.mux.HandleFunc("DELETE /api/swarms/{id}/members/{user}", s.handleSwarmMemberDelete)
}

func (s *Server) effectiveSwarmRole(r *http.Request, swarmID string) (string, error) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return "", app.ErrNotAllowed
	}
	if user.IsAdmin {
		return app.RoleOwner, nil
	}
	role, err := s.store.GetSwarmRole(r.Context(), swarmID, user.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", app.ErrNotFound
	}
	return role, err
}

func (s *Server) accessSwarm(w http.ResponseWriter, r *http.Request, action app.Action) (store.User, app.Swarm, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return user, app.Swarm{}, false
	}
	swarm, err := s.app.GetSwarm(user.ID, r.PathValue("id"))
	if errors.Is(err, app.ErrNotAllowed) {
		err = app.ErrNotFound
	}
	if err != nil {
		apiError(w, r, err)
		return user, app.Swarm{}, false
	}
	allowed, err := s.app.Can(user.ID, action, app.SwarmRes(swarm.ID))
	if err != nil {
		apiError(w, r, err)
		return user, app.Swarm{}, false
	}
	if !allowed {
		writeDetail(w, http.StatusForbidden, "swarm owner access required")
		return user, app.Swarm{}, false
	}
	return user, swarm, true
}

func decodeAccessBody(w http.ResponseWriter, r *http.Request, body any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	trimmed := bytes.TrimSpace(data)
	if err != nil || len(trimmed) == 0 || trimmed[0] != '{' {
		writeDetail(w, http.StatusBadRequest, "access request must be a JSON object")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		writeDetail(w, http.StatusBadRequest, "invalid access request fields")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeDetail(w, http.StatusBadRequest, "access request must contain a single JSON object")
		return false
	}
	return true
}

func (s *Server) handleSwarmMembers(w http.ResponseWriter, r *http.Request) {
	action := app.ActionViewSwarm
	if r.Method == http.MethodPost {
		action = app.ActionManageSwarmACL
	}
	user, swarm, ok := s.accessSwarm(w, r, action)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		grants, err := s.app.SwarmMembers(user.ID, swarm.ID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		type member struct {
			UserID      string `json:"user_id"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
			Disabled    bool   `json:"disabled"`
		}
		out := make([]member, 0, len(grants))
		for _, grant := range grants {
			target, err := s.store.GetUser(r.Context(), grant.UserID)
			if err != nil {
				apiError(w, r, err)
				return
			}
			display := ""
			if target.DisplayName != nil {
				display = *target.DisplayName
			}
			out = append(out, member{UserID: target.ID, Username: target.Username, DisplayName: display, Role: grant.Role, Disabled: target.Disabled})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if !decodeAccessBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Username) == "" || body.Role == "" {
		writeDetail(w, http.StatusBadRequest, "username and role are required")
		return
	}
	target, err := s.store.GetUserByUsername(r.Context(), body.Username)
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.app.SetSwarmMember(user.ID, swarm.ID, target.ID, body.Role); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSwarmMemberDelete(w http.ResponseWriter, r *http.Request) {
	user, swarm, ok := s.accessSwarm(w, r, app.ActionManageSwarmACL)
	if !ok {
		return
	}
	target, err := s.store.GetUserByUsername(r.Context(), r.PathValue("user"))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.app.RemoveSwarmMember(user.ID, swarm.ID, target.ID); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
