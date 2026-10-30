// Actor/template/worker/policy API: тонкий прокси к ateapi с прикладной
// авторизацией (#32/#33). Форма URL новая: в Python-сервере скоупом был
// проект, здесь им служит рой — каждый рой и есть atespace (#27).
// Объекты ходят каноническим protojson ateapi (substrate.Marshal):
// панель видит и сохраняет поля, которых формы не знают. Atespace всегда
// из строки роя, никогда из тела клиента.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/kube"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

// mountSubstrateAPI регистрирует actor/template/policy маршруты роя,
// глобальные workers/policy и подключает обработчик proxy пакета
// brood/proxy под /api/swarms/{id}/proxy/...
func (s *Server) mountSubstrateAPI() {
	// Акторы роя: /api/swarms/{id}/actors[/{name}].
	s.mux.HandleFunc("GET /api/swarms/{id}/actors", s.handleSwarmActors)
	s.mux.HandleFunc("POST /api/swarms/{id}/actors", s.handleSwarmActors)
	s.mux.HandleFunc("GET /api/swarms/{id}/actors/{name}", s.handleSwarmActor)
	s.mux.HandleFunc("PUT /api/swarms/{id}/actors/{name}", s.handleSwarmActor)
	s.mux.HandleFunc("POST /api/swarms/{id}/actors/{name}", s.handleSwarmActor)
	s.mux.HandleFunc("DELETE /api/swarms/{id}/actors/{name}", s.handleSwarmActor)

	// Шаблоны роя: /api/swarms/{id}/templates[/{name}].
	s.mux.HandleFunc("GET /api/swarms/{id}/templates", s.handleSwarmTemplates)
	s.mux.HandleFunc("POST /api/swarms/{id}/templates", s.handleSwarmTemplates)
	s.mux.HandleFunc("GET /api/swarms/{id}/templates/{name}", s.handleSwarmTemplate)
	s.mux.HandleFunc("DELETE /api/swarms/{id}/templates/{name}", s.handleSwarmTemplate)

	// Политика доступа роя: /api/swarms/{id}/policy, имя — синглтон
	// default, ставит сервер.
	s.mux.HandleFunc("GET /api/swarms/{id}/policy", s.handleSwarmPolicy)
	s.mux.HandleFunc("POST /api/swarms/{id}/policy", s.handleSwarmPolicy)
	s.mux.HandleFunc("PUT /api/swarms/{id}/policy", s.handleSwarmPolicy)
	s.mux.HandleFunc("DELETE /api/swarms/{id}/policy", s.handleSwarmPolicy)

	// Воркеры и глобальная политика — вся установка, только админ.
	s.mux.HandleFunc("GET /api/workers", s.handleWorkers)
	s.mux.HandleFunc("GET /api/workers/{name}", s.handleWorker)
	s.mux.HandleFunc("POST /api/workers/{name}", s.handleWorker)
	s.mux.HandleFunc("GET /api/policy/global", s.handleGlobalPolicy)
	s.mux.HandleFunc("POST /api/policy/global", s.handleGlobalPolicy)
	s.mux.HandleFunc("PUT /api/policy/global", s.handleGlobalPolicy)

	// Proxy к акторам (#33): Mount пишет свои пути /swarms/{id}/actors/...
	// в под-роутер, который сервер выставляет под /api/swarms/{id}/proxy/.
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

// swarmFor allows readers on GET and workload managers on mutations.
// Inaccessible swarms stay hidden as 404; writable actor proxies authorize separately.
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
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodDelete {
		if err := s.checkSwarmNamespace(r.Context(), sw); err != nil {
			apiError(w, r, err)
			return app.Swarm{}, false
		}
	}
	return sw, true
}

// clientOr503 — клиент Substrate или отказ 503 с именами настроек.
func (s *Server) clientOr503(w http.ResponseWriter, r *http.Request) (*substrate.Client, bool) {
	if r.PathValue("id") != "" {
		user, ok := currentUser(w, r)
		if !ok {
			return nil, false
		}
		if _, ok := s.swarmFor(w, r, user); !ok {
			return nil, false
		}
	}
	if s.ateapi == nil {
		writeDetail(w, http.StatusServiceUnavailable,
			"substrate is not configured: ATEAPI_TOKEN_FILE, ATEAPI_CA_FILE and ATEAPI_ENDPOINT are required")
		return nil, false
	}
	return s.ateapi, true
}

// writeMessage печатает сообщение protojson.
func writeMessage(w http.ResponseWriter, status int, message proto.Message) {
	data, err := substrate.Marshal(message)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// writeList печатает {"results": [...]} по каноническому protojson —
// та же форма, что у Python-сервера.
func writeList[T proto.Message](w http.ResponseWriter, items []T) {
	out := []json.RawMessage{}
	for _, item := range items {
		data, err := substrate.Marshal(item)
		if err != nil {
			writeDetail(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, data)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Ручная печать: элементы уже JSON, повторное кодирование удвоило бы
	// кавычки.
	_, _ = w.Write([]byte("{\"results\":[" + joinJSON(out) + "]}\n"))
}

// joinJSON склеивает JSON-фрагменты через запятую.
func joinJSON(parts []json.RawMessage) string {
	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(part)
	}
	return b.String()
}

// maxResourceBody — потолок тела actor/template/policy: protojson этих
// ресурсов с образами и правилами на порядки больше 64 KiB профиля.
const maxResourceBody int64 = 4 << 20

// parseBody читает тело в сообщение: брак — 400 с причиной, ответ записан.
func parseBody(w http.ResponseWriter, r *http.Request, message proto.Message) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResourceBody))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "malformed request body")
		return false
	}
	if err := substrate.Unmarshal(body, message); err != nil {
		apiError(w, r, err)
		return false
	}
	return true
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

// scoped стирает atespace и имя из metadata тела: адрес объекта задаёт
// путь и строка роя, не клиент.
func scoped(message proto.Message, atespace string) {
	switch m := message.(type) {
	case *ateapi.Actor:
		if m.Metadata == nil {
			m.Metadata = &ateapi.ResourceMetadata{}
		}
		m.Metadata.Atespace = atespace
		if m.ActorTemplate != nil {
			m.ActorTemplate.Atespace = atespace
		}
		if m.SourceTag != nil {
			m.SourceTag.Atespace = atespace
		}
		m.Status = nil
	case *ateapi.ActorTemplate:
		if m.Metadata == nil {
			m.Metadata = &ateapi.ResourceMetadata{}
		}
		m.Metadata.Atespace = atespace
		m.Status = nil
	case *ateapi.AccessPolicy:
		if m.Metadata == nil {
			m.Metadata = &ateapi.ResourceMetadata{}
		}
		m.Metadata.Atespace = ""
		m.Metadata.Name = substrate.PolicyName
	}
}

// ─── Акторы роя ────────────────────────────────────────────────────────────

// handleSwarmActors — GET/POST /api/swarms/{id}/actors: список и создание.
func (s *Server) handleSwarmActors(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		actors, err := client.ListActors(r.Context(), sw.AtespaceName)
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !s.nativeReadBound(w, r, sw) {
			return
		}
		writeList(w, actors)
		return
	}
	var actor ateapi.Actor
	if !parseBody(w, r, &actor) {
		return
	}
	name := actor.GetMetadata().GetName()
	if name == "" {
		writeDetail(w, http.StatusBadRequest, "metadata.name is required")
		return
	}
	sw, ok := s.swarmFor(w, r, user)
	if !ok {
		return
	}
	scoped(&actor, sw.AtespaceName)
	operation := &nativeWorkloadOperation{server: s, context: r.Context(), swarm: sw, action: "create-actor", actor: &actor, name: name}
	if err := s.app.AdmitWorkload(user.ID, sw.ID, operation); err != nil {
		apiError(w, r, err)
		return
	}
	writeMessage(w, http.StatusCreated, operation.savedActor)
}

// handleSwarmActor — GET/PUT/POST/DELETE /api/swarms/{id}/actors/{name}:
// PUT меняет шаблон и workerSelector (uid/version прочитанного —
// предусловие), POST — suspend/resume.
func (s *Server) handleSwarmActor(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		actor, err := client.GetActor(r.Context(), sw.AtespaceName, name)
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !s.nativeReadBound(w, r, sw) {
			return
		}
		writeMessage(w, http.StatusOK, actor)
	case http.MethodPut:
		var actor ateapi.Actor
		if !parseBody(w, r, &actor) {
			return
		}
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		scoped(&actor, sw.AtespaceName)
		actor.GetMetadata().Name = name
		operation := &nativeWorkloadOperation{server: s, context: r.Context(), swarm: sw, action: "update-actor", actor: &actor, name: name}
		if err := s.app.AdmitWorkload(user.ID, sw.ID, operation); err != nil {
			apiError(w, r, err)
			return
		}
		writeMessage(w, http.StatusOK, operation.savedActor)
	case http.MethodPost:
		verb, ok := readAction(w, r, "suspend", "resume")
		if !ok {
			return
		}
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		var actor *ateapi.Actor
		var err error
		if verb == "suspend" {
			err = s.checkSwarmNamespace(r.Context(), sw)
			if err == nil {
				actor, err = client.SuspendActor(r.Context(), sw.AtespaceName, name)
			}
		} else {
			operation := &nativeWorkloadOperation{server: s, context: r.Context(), swarm: sw, action: "resume-actor", name: name}
			err = s.app.AdmitWorkload(user.ID, sw.ID, operation)
			actor = operation.savedActor
		}
		if err != nil {
			apiError(w, r, err)
			return
		}
		writeMessage(w, http.StatusOK, actor)
	case http.MethodDelete:
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		actor, err := client.GetActor(r.Context(), sw.AtespaceName, name)
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !s.nativeReadBound(w, r, sw) {
			return
		}
		if err := client.DeleteActorUID(r.Context(), sw.AtespaceName, name, actor.GetMetadata().GetUid()); err != nil {
			apiError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ─── Шаблоны роя ───────────────────────────────────────────────────────────

// handleSwarmTemplates — GET/POST /api/swarms/{id}/templates. Шаблон
// неизменяем (Update в ateapi нет): правка — новый шаблон.
func (s *Server) handleSwarmTemplates(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		templates, err := client.ListActorTemplates(r.Context(), sw.AtespaceName)
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !s.nativeReadBound(w, r, sw) {
			return
		}
		writeList(w, templates)
		return
	}
	var template ateapi.ActorTemplate
	if !parseBody(w, r, &template) {
		return
	}
	if template.GetMetadata().GetName() == "" {
		writeDetail(w, http.StatusBadRequest, "metadata.name is required")
		return
	}
	sw, ok := s.swarmFor(w, r, user)
	if !ok {
		return
	}
	scoped(&template, sw.AtespaceName)
	operation := &nativeWorkloadOperation{server: s, context: r.Context(), swarm: sw, action: "create-template", template: &template}
	if err := s.app.AdmitWorkload(user.ID, sw.ID, operation); err != nil {
		apiError(w, r, err)
		return
	}
	writeMessage(w, http.StatusCreated, operation.savedTemplate)
}

// handleSwarmTemplate — GET/DELETE /api/swarms/{id}/templates/{name}.
func (s *Server) handleSwarmTemplate(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	sw, ok := s.swarmFor(w, r, user)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		template, err := client.GetActorTemplate(r.Context(), sw.AtespaceName, name)
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !s.nativeReadBound(w, r, sw) {
			return
		}
		writeMessage(w, http.StatusOK, template)
		return
	}
	operation := &nativeWorkloadOperation{server: s, context: r.Context(), swarm: sw, action: "delete-template", name: name}
	if err := s.app.AdmitWorkload(user.ID, sw.ID, operation); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Политика доступа роя ──────────────────────────────────────────────────

// handleSwarmPolicy serves the administrator-only native policy of a swarm.
// PUT preserves the read UID/version precondition; DELETE removes only the
// local policy, leaving global roles in effect.
func (s *Server) handleSwarmPolicy(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		policy, err := client.GetAtespaceAccessPolicy(r.Context(), sw.AtespaceName)
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !s.nativeReadBound(w, r, sw) {
			return
		}
		writeMessage(w, http.StatusOK, policy)
		return
	}
	if r.Method == http.MethodDelete {
		sw, ok := s.swarmFor(w, r, user)
		if !ok {
			return
		}
		if err := client.DeleteAtespaceAccessPolicy(r.Context(), sw.AtespaceName); err != nil {
			apiError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var policy ateapi.AccessPolicy
	if !parseBody(w, r, &policy) {
		return
	}
	sw, ok := s.swarmFor(w, r, user)
	if !ok {
		return
	}
	if err := s.checkSwarmNamespace(r.Context(), sw); err != nil {
		apiError(w, r, err)
		return
	}
	scoped(&policy, sw.AtespaceName)
	var saved *ateapi.AccessPolicy
	var err error
	if r.Method == http.MethodPost {
		saved, err = client.CreateAtespaceAccessPolicy(r.Context(), sw.AtespaceName, &policy)
	} else {
		saved, err = client.UpdateAtespaceAccessPolicy(r.Context(), sw.AtespaceName, &policy)
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	if r.Method == http.MethodPost {
		writeMessage(w, http.StatusCreated, saved)
		return
	}
	writeMessage(w, http.StatusOK, saved)
}

// ─── Воркеры установки ─────────────────────────────────────────────────────

// handleWorkers — GET /api/workers: зеркала подов, пишет
// ate-controller; доступ — только активный админ.
func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	workers, err := client.ListWorkers(r.Context())
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeList(w, workers)
}

// handleWorker — GET/POST /api/workers/{name}: POST — drain или replace.
// drain окончателен: осушенная запись не возвращается в работу, а её под живёт
// дальше и держит место пула. replace — выход из этого состояния: под удаляется,
// и Deployment заводит новый, чья запись регистрируется заново.
func (s *Server) handleWorker(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if r.Method == http.MethodGet {
		worker, err := client.GetWorker(r.Context(), name)
		if err != nil {
			apiError(w, r, err)
			return
		}
		writeMessage(w, http.StatusOK, worker)
		return
	}
	action, ok := readAction(w, r, "drain", "replace")
	if !ok {
		return
	}
	if action == "replace" {
		s.replaceWorker(w, r, client, name)
		return
	}
	worker, err := client.DrainWorker(r.Context(), name)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeMessage(w, http.StatusOK, worker)
}

// replaceWorker удаляет под воркера, которого планировщик уже не использует.
// Здоровый воркер не заменяется: его акторы прервались бы без причины, и
// порядок остаётся прежним — сперва drain, потом замена.
func (s *Server) replaceWorker(w http.ResponseWriter, r *http.Request, client *substrate.Client, name string) {
	if s.pools == nil {
		writeDetail(w, http.StatusServiceUnavailable,
			"worker pod access is not configured: check KUBE_API_URL, KUBE_API_TOKEN and KUBE_API_CA")
		return
	}
	worker, err := client.GetWorker(r.Context(), name)
	if err != nil {
		apiError(w, r, err)
		return
	}
	if worker.GetStatus().GetState() != ateapi.WorkerState_WORKER_STATE_DRAINING {
		writeDetail(w, http.StatusConflict,
			"only a drained worker is replaced here; drain it first if you mean to retire it")
		return
	}
	allocated := worker.GetStatus().GetAllocated()
	if allocated.GetActors() != 0 || allocated.GetCpuMillicores() != 0 || allocated.GetMemoryBytes() != 0 {
		writeDetail(w, http.StatusConflict,
			"this drained worker still has actor allocations; replacement is blocked to preserve its actors")
		return
	}
	if worker.GetWorkerNamespace() != kube.Namespace {
		writeDetail(w, http.StatusConflict,
			"this worker runs outside the installation namespace and is not replaced from here")
		return
	}
	if err := s.pools.ReplaceWorkerPod(r.Context(), worker.GetWorkerPod(), worker.GetWorkerPodUid()); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Глобальная политика ───────────────────────────────────────────────────

// handleGlobalPolicy — GET/POST/PUT /api/policy/global: синглтон всей
// установки, только админ. DELETE нет: глобальную ateapi не удаляет.
func (s *Server) handleGlobalPolicy(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		policy, err := client.GetGlobalAccessPolicy(r.Context())
		if err != nil {
			apiError(w, r, err)
			return
		}
		writeMessage(w, http.StatusOK, policy)
		return
	}
	var policy ateapi.AccessPolicy
	if !parseBody(w, r, &policy) {
		return
	}
	scoped(&policy, "")
	var saved *ateapi.AccessPolicy
	var err error
	if r.Method == http.MethodPost {
		saved, err = client.CreateGlobalAccessPolicy(r.Context(), &policy)
	} else {
		saved, err = client.UpdateGlobalAccessPolicy(r.Context(), &policy)
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	if r.Method == http.MethodPost {
		writeMessage(w, http.StatusCreated, saved)
		return
	}
	writeMessage(w, http.StatusOK, saved)
}
