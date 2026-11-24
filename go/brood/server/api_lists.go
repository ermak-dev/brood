// Плоские списки панели: рои и агенты, которые видит вызывающий.
// Администратор видит все живые рои, пользователь — свои.
package server

import (
	"context"
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
func (s *Server) visibleSwarms(ctx context.Context, user store.User) ([]app.Swarm, map[string]string, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[string]string, len(projects))
	for _, project := range projects {
		names[project.ID] = project.Name
	}
	rows, err := s.store.ListSwarms(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []app.Swarm
	for _, row := range rows {
		if row.State != string(app.StateDeleted) && (user.IsAdmin || row.OwnerID == user.ID) {
			out = append(out, app.SwarmOf(row))
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
	swarms, projects, err := s.visibleSwarms(r.Context(), user)
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
		row, err := s.swarmWithPublicKey(r.Context(), sw, username)
		if err != nil {
			apiError(w, r, err)
			return
		}
		out = append(out, swarmListJSON{swarmJSON: row, ProjectName: projects[sw.ProjectID], AgentCount: summaries[sw.ID].AgentCount})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentList — GET /api/agents: агенты всех доступных роёв.
func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	swarms, projects, err := s.visibleSwarms(r.Context(), user)
	if err != nil {
		apiError(w, r, err)
		return
	}
	agents := []agentJSON{}
	for _, sw := range swarms {
		rows, err := s.agentsJSON(r.Context(), sw, projects[sw.ProjectID])
		if err != nil {
			apiError(w, r, err)
			return
		}
		agents = append(agents, rows...)
	}
	writeJSON(w, http.StatusOK, agents)
}
