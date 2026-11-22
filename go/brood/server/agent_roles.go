package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func (s *Server) mountAgentRoles() {
	for _, pattern := range []string{"GET /api/projects/{id}/roles", "POST /api/projects/{id}/roles", "PUT /api/projects/{id}/roles/{roleID}", "DELETE /api/projects/{id}/roles/{roleID}"} {
		s.mux.HandleFunc(pattern, s.handleAgentRoles)
	}
}

func (s *Server) handleAgentRoles(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	ctx, projectID := r.Context(), r.PathValue("id")
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(projectID))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, 404, "Project not found; nothing changed")
		return
	}
	if r.Method == http.MethodGet {
		roles, err := s.store.ListAgentRoles(ctx, projectID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !user.IsAdmin {
			visible := roles[:0]
			for _, role := range roles {
				if role.RequiresDelegation() {
					continue
				}
				role.Agents, role.Prompt = []string{}, ""
				visible = append(visible, role)
			}
			roles = visible
		}
		writeJSON(w, 200, roles)
		return
	}
	if !s.freshAdmin(ctx, user.ID) {
		writeDetail(w, 403, "Only an administrator can edit agent roles; nothing changed")
		return
	}
	role := store.AgentRole{ID: r.PathValue("roleID")}
	if r.Method != http.MethodDelete {
		var input struct {
			Name        string   `json:"name"`
			Prompt      string   `json:"prompt"`
			Permissions []string `json:"permissions"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
			writeDetail(w, 400, "Expected a role name, prompt, and permissions; nothing changed")
			return
		}
		role.Name, role.Prompt, role.Permissions = strings.TrimSpace(input.Name), input.Prompt, input.Permissions
		if role.Name == "" || len(role.Name) > 120 || len(role.Prompt) > 24<<10 {
			writeDetail(w, 400, "Choose a name of 1 to 120 bytes and a prompt of at most 24 KiB; nothing changed")
			return
		}
		seen := map[string]bool{}
		for _, permission := range role.Permissions {
			key, _, scoped := strings.Cut(permission, ":")
			spec, exists := command.Lookup(key)
			valid := exists && spec.Guest && ((key == "send" || key == "tail") == scoped)
			if scoped {
				valid = valid && slices.Contains([]string{"send:agents", "send:operator", "tail:self", "tail:swarm"}, permission)
			}
			if !valid || seen[permission] {
				writeDetail(w, 400, "Choose only known, distinct role permissions; nothing changed")
				return
			}
			seen[permission] = true
		}
		if seen["tail:self"] && seen["tail:swarm"] {
			writeDetail(w, 400, "Choose one terminal access scope; nothing changed")
			return
		}
	}
	s.managedRoleWrite(ctx, w, r, user.ID, projectID, role)
}

func (s *Server) managedRoleWrite(ctx context.Context, w http.ResponseWriter, r *http.Request, userID, projectID string, role store.AgentRole) {
	if s.managed != nil {
		s.managed.admission.Lock()
		defer s.managed.admission.Unlock()
	}
	if !s.freshAdmin(ctx, userID) {
		writeDetail(w, 403, "Administrator access changed; nothing changed")
		return
	}
	var err error
	switch r.Method {
	case http.MethodPost:
		role, err = s.store.CreateAgentRole(ctx, projectID, role)
	case http.MethodPut:
		var current store.AgentRole
		current, err = s.store.GetAgentRole(ctx, projectID, role.ID)
		if err == nil {
			role.Default = current.Default
			err = s.store.UpdateAgentRole(ctx, projectID, role)
		}
	case http.MethodDelete:
		err = s.store.DeleteAgentRole(ctx, projectID, role.ID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, 404, "Role or project not found; nothing changed")
	case store.IsUniqueViolation(err):
		writeDetail(w, 409, "Choose a different role name; nothing changed")
	case errors.Is(err, store.ErrInvalidAgentRole):
		writeDetail(w, 400, err.Error()+"; nothing changed")
	case errors.Is(err, store.ErrAgentRoleDefault), errors.Is(err, store.ErrAgentRoleInUse), errors.Is(err, store.ErrAgentRoleDelegation):
		writeDetail(w, 409, err.Error()+"; nothing changed")
	case err != nil:
		apiError(w, r, err)
	case r.Method == http.MethodDelete:
		w.WriteHeader(204)
	default:
		updated, err := s.store.GetAgentRole(ctx, projectID, role.ID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		writeJSON(w, status, updated)
	}
}

func (m *managedProvider) authorizeGuestPermission(ctx context.Context, from store.ManagedActor, projectID, permission string) error {
	role, err := m.server.store.GetAgentRole(ctx, projectID, from.Role)
	if err != nil || (!slices.Contains(role.Permissions, permission) && !(permission == "tail:self" && slices.Contains(role.Permissions, "tail:swarm"))) {
		return command.Refuse(app.ErrNotAllowed, "This agent role does not allow "+permission+"; nothing changed. Ask the operator to review the role.")
	}
	switch permission {
	case "tail:swarm", "add", "start", "stop", "restart", "slash", "reset", "rm":
		return m.authorizeMaster(ctx, from)
	}
	return nil
}

func (s *Server) agentRoleName(ctx context.Context, projectID, roleID string) string {
	role, err := s.store.GetAgentRole(ctx, projectID, roleID)
	if err != nil {
		return "Unavailable"
	}
	return role.Name
}

func (m *managedProvider) guestToolList(ctx context.Context, from store.ManagedActor, projectID string) relay.SwarmResponse {
	role, err := m.server.store.GetAgentRole(ctx, projectID, from.Role)
	if err != nil {
		return relay.SwarmResponse{Text: "Agent role unavailable; retry"}
	}
	delegated := from.DelegatorID != ""
	if delegated {
		u, err := m.server.store.GetUser(ctx, from.DelegatorID)
		delegated = err == nil && !u.Disabled && u.IsAdmin && m.server.authorizeSwarmManagerIntent(ctx, u.ID, from.SwarmID) == nil
	}
	tools := []string{}
	for _, spec := range command.Specs() {
		if !spec.Guest {
			continue
		}
		var enabled bool
		switch spec.ToolName {
		case "send":
			enabled = slices.Contains(role.Permissions, "send:agents") || slices.Contains(role.Permissions, "send:operator")
		case "tail":
			enabled = slices.Contains(role.Permissions, "tail:self") || slices.Contains(role.Permissions, "tail:swarm")
		case "agents", "diagnose":
			enabled = slices.Contains(role.Permissions, spec.ToolName)
		case "add", "start", "stop", "restart", "slash", "reset", "rm":
			enabled = delegated && slices.Contains(role.Permissions, spec.ToolName)
		}
		if enabled {
			tools = append(tools, spec.ToolName)
		}
	}
	data, _ := json.Marshal(tools)
	return relay.SwarmResponse{OK: true, Text: "Agent tools listed", Data: data, Outcome: "completed"}
}
