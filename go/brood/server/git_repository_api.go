package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/store"
)

const projectRepositoryBodyLimit = 4 << 10

type projectRepositoryJSON struct {
	Origin string `json:"origin"`
}

func (s *Server) mountGitRepositories() {
	s.mux.HandleFunc("PUT /api/projects/sync", s.handleProjectSync)
	s.mux.HandleFunc("GET /api/projects/{id}/repository", s.handleProjectRepository)
	s.mux.HandleFunc("PUT /api/projects/{id}/repository", s.handleProjectRepositoryUpdate)
}

// freshAdmin перечитывает пользователя: роль в контексте запроса могла устареть.
func (s *Server) freshAdmin(ctx context.Context, id string) bool {
	user, err := s.store.GetUser(ctx, id)
	return err == nil && !user.Disabled && user.IsAdmin
}

// handleProjectRepository — GET /api/projects/{id}/repository для участников проекта.
func (s *Server) handleProjectRepository(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	allowed, err := s.app.Can(r.Context(), user.ID, app.ActionViewProject, app.ProjectRes(id))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	origin, err := s.store.ProjectRepository(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "project repository unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, projectRepositoryJSON{Origin: origin})
}

// handleProjectRepositoryUpdate — PUT /api/projects/{id}/repository: только
// администратор (CSRF проверяет middleware) задаёт SSH или HTTPS origin или "".
// Сессию и роль транзакция перечитывает после чтения тела, до записи.
func (s *Server) handleProjectRepositoryUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !s.freshAdmin(r.Context(), user.ID) {
		writeDetail(w, http.StatusForbidden, "administrator access required for project repositories")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetProject(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	} else if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "project repository unavailable")
		return
	}
	origin, ok := readProjectRepositoryBody(w, r)
	if !ok {
		return
	}
	if origin != "" {
		canonical, err := gitorigin.FromRemote(origin)
		if err != nil {
			writeDetail(w, http.StatusUnprocessableEntity, "invalid project repository: "+err.Error())
			return
		}
		origin = canonical
	}
	err := s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if !secretRequestSession(ctx, s, r, user.ID) || !s.freshAdmin(ctx, user.ID) {
			return app.ErrNotAllowed
		}
		return s.store.SetProjectRepository(ctx, id, origin)
	})
	switch {
	case errors.Is(err, app.ErrNotAllowed):
		writeDetail(w, http.StatusForbidden, "project repository authorization changed")
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "project not found")
	case err != nil:
		writeDetail(w, http.StatusServiceUnavailable, "project repository update failed")
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, projectRepositoryJSON{Origin: origin})
	}
}

// readProjectRepositoryBody принимает ровно {"origin":"<string>"}: точное имя
// поля без регистровых вариантов, без повторов, лишних полей и документов.
// Отказ не повторяет тело: origin может содержать userinfo.
func readProjectRepositoryBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, projectRepositoryBodyLimit))
	_ = control.SetReadDeadline(time.Time{})
	if err != nil || !utf8.Valid(data) {
		writeDetail(w, http.StatusBadRequest, "invalid or oversized project repository request")
		return "", false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	invalid := func() (string, bool) {
		writeDetail(w, http.StatusBadRequest, `project repository request must be exactly {"origin":"<string>"}`)
		return "", false
	}
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return invalid()
	}
	if token, err := d.Token(); err != nil || token != "origin" {
		return invalid()
	}
	token, err := d.Token()
	origin, isString := token.(string)
	if err != nil || !isString {
		return invalid()
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	return origin, true
}
