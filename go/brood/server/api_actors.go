// API воркеров роя: /api/swarms/{id}/actors и /api/swarms/{id}/templates.
// Воркер — контейнер Proxmox; шаблон — профиль включённого пула.
package server

import (
	"errors"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// maxResourceBody — потолок тела запроса создания воркера.
const maxResourceBody int64 = 1 << 20

// actorInput — тело создания воркера: имя и шаблон пула. Остальное задаёт
// профиль пула, клиент его не переопределяет.
type actorInput struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	ActorTemplate struct {
		Name string `json:"name"`
	} `json:"actorTemplate"`
}

func (s *Server) mountActorAPI() {
	s.mux.HandleFunc("GET /api/swarms/{id}/actors", s.handleSwarmActors)
	s.mux.HandleFunc("POST /api/swarms/{id}/actors", s.handleSwarmActors)
	s.mux.HandleFunc("GET /api/swarms/{id}/actors/{name}", s.handleSwarmActor)
	s.mux.HandleFunc("PUT /api/swarms/{id}/actors/{name}", s.handleSwarmActor)
	s.mux.HandleFunc("POST /api/swarms/{id}/actors/{name}", s.handleSwarmActor)
	s.mux.HandleFunc("DELETE /api/swarms/{id}/actors/{name}", s.handleSwarmActor)
	s.mux.HandleFunc("GET /api/swarms/{id}/templates", s.handleSwarmTemplates)
	s.mux.HandleFunc("GET /api/swarms/{id}/templates/{name}", s.handleSwarmTemplate)

	// Proxy к воркерам: Mount пишет свои пути /swarms/{id}/actors/... в
	// под-роутер, который сервер выставляет под /api/swarms/{id}/proxy/.
	sub := http.NewServeMux()
	s.proxy.Mount(sub)
	s.mux.Handle("/api/swarms/{id}/proxy/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			s.issueActorView(w, r)
			return
		}
		clone := r.Clone(r.Context())
		urlCopy := *r.URL
		clone.URL = &urlCopy
		clone.URL.Path = "/swarms/" + r.PathValue("id") + strings.TrimPrefix(r.URL.Path, "/api/swarms/"+r.PathValue("id")+"/proxy")
		clone.URL.RawPath = ""
		sub.ServeHTTP(w, clone)
	}))
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
	ok, err := s.app.Can(user.ID, action, app.SwarmRes(swarmID))
	if err != nil {
		apiError(w, r, err)
		return app.Swarm{}, false
	}
	if !ok {
		writeDetail(w, http.StatusNotFound, "swarm not found")
		return app.Swarm{}, false
	}
	sw, found, err := s.gateway.SwarmByID(swarmID)
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

// handleSwarmActors — GET/POST /api/swarms/{id}/actors.
func (s *Server) handleSwarmActors(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		actors, err := s.managed.actorsJSON(r.Context(), sw)
		if err != nil {
			apiError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": actors})
		return
	}
	var input actorInput
	creation, parsed := parseActorCreation(w, r, &input)
	if !parsed {
		return
	}
	if input.Metadata.Name == "" {
		writeDetail(w, http.StatusBadRequest, "metadata.name is required")
		return
	}
	sw, ok := s.swarmFor(w, r, user)
	if !ok {
		return
	}
	pool, err := s.managed.templatePool(r.Context(), input.ActorTemplate.Name)
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusBadRequest, "actorTemplate.name must name an enabled Proxmox pool profile")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	s.createManagedActor(w, r, user.ID, sw, input.Metadata.Name, pool, creation)
}

// handleSwarmActor — GET/PUT/POST/DELETE /api/swarms/{id}/actors/{name}.
func (s *Server) handleSwarmActor(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	sw, allowed := s.swarmFor(w, r, user)
	if !allowed {
		return
	}
	a, err := s.managed.actorByName(r.Context(), sw.ID, r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "actor not found")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	r = r.WithContext(s.webCommandContext(r, user.ID))
	s.handleManagedActor(w, r, user.ID, sw, a)
}

// handleSwarmTemplates — GET /api/swarms/{id}/templates: профили включённых пулов.
func (s *Server) handleSwarmTemplates(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if _, ok := s.swarmFor(w, r, user); !ok {
		return
	}
	templates, err := s.managedTemplates(r.Context())
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": templates})
}

// handleSwarmTemplate — GET /api/swarms/{id}/templates/{name}.
func (s *Server) handleSwarmTemplate(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if _, ok := s.swarmFor(w, r, user); !ok {
		return
	}
	pool, err := s.managed.templatePool(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "template not found")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, managedTemplateJSON(pool))
}
