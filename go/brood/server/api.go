// HTTP API проектов, членства и роёв: тонкий слой над прикладным
// сервисом app. Статусы: 400 брак тела, 401 аноним, 403 запрет, 404 нет
// или скрыто, 409 конфликт, 503 отсутствует настройка сервиса. Ошибки —
// JSON {"detail": "<english>"}.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

// projectJSON — публичная форма проекта.
type projectJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// projectRowJSON — строка списка проектов: проект и число роёв, которые
// вызывающий видит в его списке роёв (#234).
type projectRowJSON struct {
	projectJSON
	SwarmCount int64 `json:"swarmCount"`
	// Поля плоской таблицы проектов: репозиторий, участники и лимиты одним
	// запросом, без обращения за каждым проектом отдельно.
	Repository string       `json:"repository"`
	Members    []string     `json:"members"`
	Capacity   capacityJSON `json:"capacity"`
}

// swarmRowJSON — строка списка роёв: рой и число его живых воркеров.
type swarmRowJSON struct {
	swarmJSON
	ManagedActorCount int64 `json:"managedActorCount"`
}

// swarmJSON — публичная форма роя.
type swarmJSON struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	OwnerID       string `json:"owner_id"`
	OwnerUsername string `json:"owner_username"`
	SSHPublicKey  string `json:"sshPublicKey"`
	Role          string `json:"role"`
	Name          string `json:"name"`
	State         string `json:"state"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// toSwarmJSON приводит роя к публичной форме; время — UTC RFC3339.
func toSwarmJSON(sw app.Swarm, ownerUsername string) swarmJSON {
	return swarmJSON{
		ID: sw.ID, ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, OwnerUsername: ownerUsername, Name: sw.Name,
		State: string(sw.State), Role: "owner",
		CreatedAt: sw.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: sw.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// swarmWithPublicKey дополняет ответ для роя, к которому уже проверен доступ.
func (s *Server) swarmWithPublicKey(ctx context.Context, sw app.Swarm, ownerUsername string) (swarmJSON, error) {
	key, err := s.store.GetSwarmSSHPublicKey(ctx, sw.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return swarmJSON{}, err
	}
	out := toSwarmJSON(sw, ownerUsername)
	out.SSHPublicKey = key
	return out, nil
}

// writeJSON пишет успешный ответ JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeDetail пишет ошибку контракта: {"detail": "<english>"}.
func writeDetail(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, struct {
		Detail string `json:"detail"`
	}{Detail: detail})
}

// publicDetail снимает с текста отказа служебное имя причины ("conflict: "):
// человеку нужна сама причина, статус HTTP несёт её род.
func publicDetail(err error) string {
	detail := err.Error()
	for _, sentinel := range []error{app.ErrConflict, app.ErrExists, app.ErrNotFound, app.ErrNotAllowed, app.ErrInvalidWorkload, app.ErrInvalidCapacity} {
		if rest, ok := strings.CutPrefix(detail, sentinel.Error()+": "); ok {
			return rest
		}
	}
	return detail
}

// apiError переводит отказ домена в статус HTTP. Внутренние сбои
// логируются с причиной, наружу — только «internal error».
func apiError(w http.ResponseWriter, r *http.Request, err error) {
	detail := publicDetail(err)
	var status int
	var limitError *app.LimitExceededError
	switch {
	case errors.Is(err, app.ErrInvalidWorkload), errors.Is(err, app.ErrInvalidCapacity):
		status = http.StatusBadRequest
	case errors.As(err, &limitError):
		status = http.StatusConflict
	case errors.Is(err, app.ErrNotAllowed):
		status = http.StatusForbidden
	case errors.Is(err, app.ErrNotFound), errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, app.ErrExists), errors.Is(err, app.ErrConflict), errors.Is(err, store.ErrProjectHasSwarms):
		status = http.StatusConflict
	default:
		var apiErr *app.APIError
		var cfgErr *ConfigError
		switch {
		case errors.As(err, &apiErr):
			status, detail = apiErr.Status, apiErr.Detail
		case errors.As(err, &cfgErr):
			status, detail = http.StatusServiceUnavailable, cfgErr.Error()
		default:
			if lg := auth.LoggerFromContext(r.Context()); lg != nil {
				lg.Error("internal error", "err", err.Error())
			} else {
				slog.Error("internal error", "err", err.Error())
			}
			writeDetail(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	writeDetail(w, status, detail)
}

// currentUser достаёт пользователя запроса; сам пишет 401 при анониме.
func currentUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeDetail(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	return user, true
}

// decodeBody читает ограниченное JSON-тело в v; при браке пишет 400.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		writeDetail(w, http.StatusBadRequest, "malformed JSON body")
		return false
	}
	return true
}

// ─── Проекты ───────────────────────────────────────────────────────────────

// handleProjects — GET/POST /api/projects: список и создание. Список —
// проекты членства вызывающего; админу доступны все.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		projects, err := s.app.ListProjectsFor(r.Context(), user.ID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		s.writeProjects(w, r, user.ID, projects)
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.Name == "" {
			writeDetail(w, http.StatusBadRequest, "name is required")
			return
		}
		project, err := s.app.CreateProject(r.Context(), user.ID, body.Name)
		if err != nil {
			apiError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, projectJSON{ID: project.ID, Name: project.Name})
	}
}

// writeProjects пишет список проектов с числом роёв каждого; пустой список
// — []. Число роёв — одна выборка на весь список по правилу списка роёв
// проекта, а не запрос на проект.
func (s *Server) writeProjects(w http.ResponseWriter, r *http.Request, userID string, projects []store.Project) {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	counts, err := s.store.ProjectSwarmCounts(r.Context(), userID, ids)
	if err != nil {
		apiError(w, r, err)
		return
	}
	out := make([]projectRowJSON, 0, len(projects))
	for _, p := range projects {
		row := projectRowJSON{projectJSON: projectJSON{ID: p.ID, Name: p.Name}, SwarmCount: counts[p.ID], Members: []string{}}
		if row.Repository, err = s.store.ProjectRepository(r.Context(), p.ID); err != nil {
			apiError(w, r, err)
			return
		}
		members, err := s.store.MembersOf(r.Context(), p.ID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		for _, member := range members {
			row.Members = append(row.Members, member.Username)
		}
		limits, err := s.store.GetProjectCapacity(r.Context(), p.ID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		row.Capacity = formatCapacity(limits)
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleProjectDelete — DELETE /api/projects/{id}: только админ.
func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	id := r.PathValue("id")
	err := s.store.WithTx(r.Context(), func(ctx context.Context) error {
		current, err := s.store.GetUser(ctx, user.ID)
		if errors.Is(err, store.ErrNotFound) {
			return app.ErrNotAllowed
		}
		if err != nil {
			return err
		}
		if current.Disabled || !current.IsAdmin {
			return app.ErrNotAllowed
		}
		// Образ проекта — контейнер в PVE: его удаляют явно, до проекта.
		if images, err := s.store.ProjectHasImages(ctx, id); err != nil {
			return err
		} else if images {
			return &app.APIError{Status: http.StatusConflict, Detail: "the project has images; delete them first"}
		}
		return s.store.DeleteProjectWithoutSwarms(ctx, id)
	})
	if err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMembers — POST /api/projects/{id}/members {"username"}: только
// админ.
func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	var body struct {
		Username string `json:"username"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Username == "" {
		writeDetail(w, http.StatusBadRequest, "username is required")
		return
	}
	projectID := r.PathValue("id")
	target, err := s.store.GetUserByUsername(r.Context(), body.Username)
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.app.AddMember(r.Context(), user.ID, projectID, target.ID); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMemberDelete — DELETE /api/projects/{id}/members/{user}: только
// админ; {user} — username.
func (s *Server) handleMemberDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	projectID := r.PathValue("id")
	target, err := s.store.GetUserByUsername(r.Context(), r.PathValue("user"))
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.app.RemoveMember(r.Context(), user.ID, projectID, target.ID); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProjectSwarms — GET/POST /api/projects/{id}/swarms: собственные
// рои вызывающего в проекте; POST создаёт рой.
func (s *Server) handleProjectSwarms(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	projectID := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		swarms, err := s.app.ListSwarmsFor(r.Context(), user.ID, projectID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		s.writeSwarms(w, r, swarms)
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.Name == "" {
			writeDetail(w, http.StatusBadRequest, "name is required")
			return
		}
		sw, err := s.app.CreateSwarm(r.Context(), user.ID, projectID, body.Name)
		if err != nil {
			apiError(w, r, err)
			return
		}
		s.writeSwarm(w, r, http.StatusCreated, sw)
	}
}

// writeSwarms пишет список роёв со сводкой исполнения каждого; пустой
// список — []. Сводка — одна выборка на весь список и только по роям,
// которые список уже показывает вызывающему. Рой без сводки исчез из базы
// между чтениями: его больше нет, и в список он не попадает.
func (s *Server) writeSwarms(w http.ResponseWriter, r *http.Request, swarms []app.Swarm) {
	ids := make([]string, 0, len(swarms))
	for _, sw := range swarms {
		ids = append(ids, sw.ID)
	}
	summaries, err := s.store.SwarmSummaries(r.Context(), ids)
	if err != nil {
		apiError(w, r, err)
		return
	}
	out := make([]swarmRowJSON, 0, len(swarms))
	owners := make(map[string]string)
	for _, sw := range swarms {
		summary, ok := summaries[sw.ID]
		if !ok {
			continue
		}
		username, ok := owners[sw.OwnerID]
		if !ok {
			owner, err := s.store.GetUser(r.Context(), sw.OwnerID)
			if err != nil {
				apiError(w, r, err)
				return
			}
			username = owner.Username
			owners[sw.OwnerID] = username
		}
		row, err := s.swarmWithPublicKey(r.Context(), sw, username)
		if err != nil {
			apiError(w, r, err)
			return
		}
		out = append(out, swarmRowJSON{swarmJSON: row, ManagedActorCount: summary.ManagedActorCount})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) writeSwarm(w http.ResponseWriter, r *http.Request, status int, sw app.Swarm) {
	owner, err := s.store.GetUser(r.Context(), sw.OwnerID)
	if err != nil {
		apiError(w, r, err)
		return
	}
	out, err := s.swarmWithPublicKey(r.Context(), sw, owner.Username)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, status, out)
}

// handleSwarm — GET/DELETE /api/swarms/{id}: рой с проверкой доступа.
func (s *Server) handleSwarm(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		sw, err := s.app.GetSwarm(r.Context(), user.ID, r.PathValue("id"))
		if errors.Is(err, app.ErrNotAllowed) {
			// Чужой рой не выделяется из отсутствующего: существование
			// роя не раскрывается не-владельцу.
			err = fmt.Errorf("%w: swarm not found", app.ErrNotFound)
		}
		if err != nil {
			apiError(w, r, err)
			return
		}
		s.writeSwarm(w, r, http.StatusOK, sw)
	case http.MethodDelete:
		if err := s.app.DeleteSwarm(r.Context(), user.ID, r.PathValue("id")); err != nil {
			apiError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleSwarmTransfer allows owners and administrators to transfer an active swarm.
// The domain layer validates the target and rechecks the grant inside the write.
func (s *Server) handleSwarmTransfer(w http.ResponseWriter, r *http.Request) {
	user, swarm, ok := s.accessSwarm(w, r)
	if !ok {
		return
	}
	var body struct {
		Username string `json:"username"`
	}
	if !decodeAccessBody(w, r, &body) {
		return
	}
	if body.Username == "" {
		writeDetail(w, http.StatusBadRequest, "username is required")
		return
	}
	target, err := s.store.GetUserByUsername(r.Context(), body.Username)
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.app.TransferSwarm(r.Context(), user.ID, swarm.ID, target.ID); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminUsers — GET /api/admin/users: список пользователей, только
// админ; без хэшей паролей.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		apiError(w, r, err)
		return
	}
	type row struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		IsAdmin     bool   `json:"is_admin"`
		Disabled    bool   `json:"disabled"`
	}
	out := make([]row, 0, len(users))
	for _, u := range users {
		display := ""
		if u.DisplayName != nil {
			display = *u.DisplayName
		}
		out = append(out, row{Username: u.Username, DisplayName: display,
			IsAdmin: u.IsAdmin, Disabled: u.Disabled})
	}
	writeJSON(w, http.StatusOK, out)
}
