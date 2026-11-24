// API агентов роя: /api/swarms/{id}/agents и /api/swarms/{id}/profiles.
// Агент — контейнер Proxmox; профиль — включённый пул, в котором его создают.
package server

import (
	"errors"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// maxResourceBody — потолок тела запроса создания агента.
const maxResourceBody int64 = 1 << 20

func (s *Server) mountAgentAPI() {
	s.mux.HandleFunc("GET /api/swarms/{id}/agents", s.handleSwarmAgents)
	s.mux.HandleFunc("POST /api/swarms/{id}/agents", s.handleSwarmAgents)
	s.mux.HandleFunc("GET /api/swarms/{id}/agents/{name}", s.handleSwarmAgent)
	s.mux.HandleFunc("POST /api/swarms/{id}/agents/{name}", s.handleSwarmAgent)
	s.mux.HandleFunc("DELETE /api/swarms/{id}/agents/{name}", s.handleSwarmAgent)
	s.mux.HandleFunc("GET /api/swarms/{id}/profiles", s.handleSwarmProfiles)

	// Терминал агента: пропуск и прямой поток WebSocket.
	s.mux.HandleFunc("/api/swarms/{id}/proxy/agents/{name}", s.handleAgentTerminal)
	s.mux.HandleFunc("/api/swarms/{id}/proxy/agents/{name}/{path...}", s.handleAgentTerminal)
}

// swarmFor пропускает владельца роя и администратора. Недоступный рой
// скрыт как 404.
func (s *Server) swarmFor(w http.ResponseWriter, r *http.Request, user store.User) (app.Swarm, bool) {
	action := app.ActionManageSwarm
	if r.Method == http.MethodGet {
		action = app.ActionViewSwarm
	}
	return s.swarmForAction(w, r, user, action)
}

func (s *Server) swarmForAction(w http.ResponseWriter, r *http.Request, user store.User, action app.Action) (app.Swarm, bool) {
	swarmID := r.PathValue("id")
	ok, err := s.app.Can(r.Context(), user.ID, action, app.SwarmRes(swarmID))
	if err != nil {
		apiError(w, r, err)
		return app.Swarm{}, false
	}
	if !ok {
		writeDetail(w, http.StatusNotFound, "swarm not found")
		return app.Swarm{}, false
	}
	sw, found, err := s.app.SwarmByID(r.Context(), swarmID)
	if err != nil {
		apiError(w, r, err)
		return app.Swarm{}, false
	}
	if !found {
		writeDetail(w, http.StatusNotFound, "swarm not found")
		return app.Swarm{}, false
	}
	return sw, true
}

// readAction разбирает тело {"action": ...}; брак — 400, ответ записан.
func readAction(w http.ResponseWriter, r *http.Request, allowed ...string) (string, bool) {
	var body struct {
		Action string `json:"action"`
	}
	if !decodeBody(w, r, &body) {
		return "", false
	}
	name := strings.ToLower(strings.TrimSpace(body.Action))
	for _, want := range allowed {
		if name == want {
			return name, true
		}
	}
	writeDetail(w, http.StatusBadRequest,
		"action must be "+strings.Join(allowed, " or "))
	return "", false
}

// handleSwarmAgents — GET/POST /api/swarms/{id}/agents.
func (s *Server) handleSwarmAgents(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		agents, err := s.agentsJSON(r.Context(), sw, s.projectName(r.Context(), sw))
		if err != nil {
			apiError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, agents)
		return
	}
	input, parsed := readAgentInput(w, r)
	if !parsed {
		return
	}
	if input.Name == "" {
		writeDetail(w, http.StatusBadRequest, "name the agent; no agent was created")
		return
	}
	if input.Pool == "" {
		writeDetail(w, http.StatusBadRequest, "choose a pool for the agent; no agent was created")
		return
	}
	sw, ok := s.swarmFor(w, r, user)
	if !ok {
		return
	}
	pool, err := s.provider.poolByName(r.Context(), input.Pool)
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusBadRequest, "pool "+input.Pool+" not found; no agent was created. Choose a pool from the list")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	s.handleCreateAgent(w, r, user.ID, sw, input, pool)
}

// handleSwarmAgent — GET/POST/DELETE /api/swarms/{id}/agents/{name}.
func (s *Server) handleSwarmAgent(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	sw, allowed := s.swarmFor(w, r, user)
	if !allowed {
		return
	}
	a, err := s.provider.actorByName(r.Context(), sw.ID, r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "agent "+r.PathValue("name")+" not found; it may have been deleted, reload the list")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	r = r.WithContext(s.webCommandContext(r, user.ID))
	s.handleAgentAction(w, r, user.ID, sw, a)
}

// handleSwarmProfiles — GET /api/swarms/{id}/profiles: имена пулов, в которых
// можно создать агента.
func (s *Server) handleSwarmProfiles(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if _, ok := s.swarmFor(w, r, user); !ok {
		return
	}
	profiles, err := s.agentProfiles(r.Context())
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}
