package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/gitcredential"
	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/store"
)

const gitCredentialBodyLimit = 16 << 10

// mountGitCredentials — серверные Git credentials проекта (#173): отдельный
// от доставляемых var/file вид секрета. Чтение — только метаданные; значения
// API не возвращает никогда.
func (s *Server) mountGitCredentials() {
	s.mux.HandleFunc("GET /api/projects/{id}/secrets/git", s.handleGitCredentials)
	s.mux.HandleFunc("PUT /api/projects/{id}/secrets/git", s.handleGitCredentialPut)
	s.mux.HandleFunc("DELETE /api/projects/{id}/secrets/git", s.handleGitCredentialDelete)
}

// handleGitCredentials — GET: имена, привязанные repositories и флаг active.
func (s *Server) handleGitCredentials(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.projectSecretAdmin(w, r); !ok {
		return
	}
	s.writeGitCredentials(w, r, "project Git credential metadata unavailable")
}

func (s *Server) writeGitCredentials(w http.ResponseWriter, r *http.Request, unavailable string) {
	infos, err := s.store.ProjectGitCredentials(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, unavailable)
		return
	}
	if !s.validProxySession(r) {
		writeDetail(w, http.StatusForbidden, "secret session authorization changed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": infos})
}

// handleGitCredentialPut — PUT ровно {"name","repository","username","token"}:
// создаёт или ротирует credential. Repository канонизируется до привязки.
func (s *Server) handleGitCredentialPut(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.projectSecretAdmin(w, r)
	if !ok {
		return
	}
	fields, ok := readGitCredentialBody(w, r, "name", "repository", "username", "token")
	if !ok {
		return
	}
	var c gitcredential.Credential
	for key, target := range map[string]*string{"name": &c.Name, "repository": &c.Repository, "username": &c.Username, "token": &c.Token} {
		if !syncString(fields[key], target) {
			writeDetail(w, http.StatusBadRequest, gitCredentialPutShape)
			return
		}
	}
	if canonical, err := gitorigin.Canonical(c.Repository); err == nil {
		c.Repository = canonical
	}
	if err := gitcredential.Validate(c); err != nil {
		writeDetail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.mutateGitCredentials(w, r, userID, func(ctx context.Context) error {
		project := r.PathValue("id")
		repository, err := s.store.ProjectRepository(ctx, project)
		if err != nil {
			return err
		}
		if err := s.store.PutProjectGitCredential(ctx, project, c); err != nil {
			return err
		}
		if repository == c.Repository {
			return s.store.SetProjectGitCredentialRef(ctx, project, c.Name)
		}
		return nil
	})
}

// handleGitCredentialDelete — DELETE ровно {"names":[...]}: 1..16 имён.
// Отсутствующие имена безвредны; ссылка декларации остаётся и ни во что не
// разрешается.
func (s *Server) handleGitCredentialDelete(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.projectSecretAdmin(w, r)
	if !ok {
		return
	}
	fields, ok := readGitCredentialBody(w, r, "names")
	if !ok {
		return
	}
	var names []string
	raw := fields["names"]
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &names) != nil || len(names) == 0 || len(names) > gitcredential.MaxPerProject {
		writeDetail(w, http.StatusBadRequest, gitCredentialDeleteShape)
		return
	}
	for _, name := range names {
		if gitcredential.ValidateName(name) != nil {
			writeDetail(w, http.StatusBadRequest, gitCredentialDeleteShape)
			return
		}
	}
	s.mutateGitCredentials(w, r, userID, func(ctx context.Context) error {
		return s.store.RemoveProjectGitCredentials(ctx, r.PathValue("id"), names)
	})
}

const (
	gitCredentialPutShape    = `Git credential request must be exactly {"name","repository","username","token"} with string values`
	gitCredentialDeleteShape = `Git credential removal must be exactly {"names":[...]} with 1 to 16 valid names`
)

// mutateGitCredentials перепроверяет сессию и роль в транзакции записи.
func (s *Server) mutateGitCredentials(w http.ResponseWriter, r *http.Request, userID string, apply func(context.Context) error) {
	err := s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if !secretRequestSession(ctx, s, r, userID) || !s.freshAdmin(ctx, userID) {
			return app.ErrNotAllowed
		}
		return apply(ctx)
	})
	switch {
	case errors.Is(err, app.ErrNotAllowed):
		writeDetail(w, http.StatusForbidden, "project Git credential authorization changed")
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "project not found")
	case errors.Is(err, store.ErrGitCredentialLimit):
		writeDetail(w, http.StatusUnprocessableEntity, store.ErrGitCredentialLimit.Error())
	case err != nil:
		writeDetail(w, http.StatusServiceUnavailable, "project Git credential update failed")
	default:
		s.writeGitCredentials(w, r, "Git credential outcome requires verification")
	}
}

// readGitCredentialBody принимает объект ровно с этими полями: точные имена
// без регистровых вариантов, без повторов, лишних полей и документов. Отказ
// не повторяет тело: в нём token.
func readGitCredentialBody(w http.ResponseWriter, r *http.Request, keys ...string) (map[string]json.RawMessage, bool) {
	shape := gitCredentialPutShape
	if len(keys) == 1 {
		shape = gitCredentialDeleteShape
	}
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gitCredentialBodyLimit))
	_ = control.SetReadDeadline(time.Time{})
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "invalid or oversized Git credential request")
		return nil, false
	}
	fields, err := proxmoxObject(string(data))
	if err != nil || len(fields) != len(keys) {
		writeDetail(w, http.StatusBadRequest, shape)
		return nil, false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			writeDetail(w, http.StatusBadRequest, shape)
			return nil, false
		}
	}
	return fields, true
}
