// Плоские списки панели: рои и агенты, которые видит вызывающий.
// Администратор видит все живые рои, пользователь — свои.
package server

import (
	"net/http"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// swarmListJSON — строка плоского списка роёв.
type swarmListJSON struct {
	swarmJSON
	ProjectName string `json:"project_name"`
	AgentCount  int64  `json:"agentCount"`
}

func (s *Server) mountLists() {
	s.mux.HandleFunc("GET /api/swarms", s.handleSwarmList)
	s.mux.HandleFunc("GET /api/agents", s.handleAgentList)
}

// visibleSwarms — живые рои, доступные пользователю, и имена их проектов.
func (s *Server) visibleSwarms(user store.User) ([]app.Swarm, map[string]string, error) {
	projects, err := s.gateway.Projects()
	if err != nil {
		return nil, nil, err
	}
	names := make(map[string]string, len(projects))
	for _, project := range projects {
		names[project.ID] = project.Name
	}
	all, err := s.gateway.Swarms()
	if err != nil {
		return nil, nil, err
	}
	out := all[:0]
	for _, sw := range all {
		if sw.State != app.StateDeleted && (user.IsAdmin || sw.OwnerID == user.ID) {
			out = append(out, sw)
		}
	}
	return out, names, nil
}

// handleSwarmList — GET /api/swarms.
func (s *Server) handleSwarmList(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	swarms, projects, err := s.visibleSwarms(user)
	if err != nil {
		apiError(w, r, err)
		return
	}
	ids := make([]string, 0, len(swarms))
	for _, sw := range swarms {
		ids = append(ids, sw.ID)
	}
	summaries, err := s.store.SwarmSummaries(r.Context(), ids)
	if err != nil {
		apiError(w, r, err)
		return
	}
	owners := map[string]string{}
	out := make([]swarmListJSON, 0, len(swarms))
	for _, sw := range swarms {
		username, known := owners[sw.OwnerID]
		if !known {
			if owner, err := s.store.GetUser(r.Context(), sw.OwnerID); err == nil {
				username = owner.Username
			}
			owners[sw.OwnerID] = username
		}
		out = append(out, swarmListJSON{swarmJSON: toSwarmJSON(sw, username), ProjectName: projects[sw.ProjectID], AgentCount: summaries[sw.ID].ManagedActorCount})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentList — GET /api/agents: агенты всех доступных роёв.
func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	swarms, projects, err := s.visibleSwarms(user)
	if err != nil {
		apiError(w, r, err)
		return
	}
	results := []any{}
	for _, sw := range swarms {
		rows, err := s.managed.actorsJSON(r.Context(), sw)
		if err != nil {
			apiError(w, r, err)
			return
		}
		for _, row := range rows {
			if agent, ok := row.(map[string]any); ok {
				agent["swarmName"] = sw.Name
				agent["projectID"] = sw.ProjectID
				agent["projectName"] = projects[sw.ProjectID]
			}
			results = append(results, row)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
