package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

func (s *Server) mountProjectOwnership() {
	s.mux.HandleFunc("GET /api/projects/{id}/owner", s.handleProjectOwner)
	s.mux.HandleFunc("PUT /api/projects/{id}/owner", s.handleProjectOwnerUpdate)
}

func projectOwnerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrProjectOwnerDenied):
		writeDetail(w, http.StatusForbidden, "project owner authorization changed or access denied")
	case errors.Is(err, store.ErrProjectOwnerConflict):
		writeDetail(w, http.StatusConflict, "project owner revision changed; reload before editing")
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "project not found")
	default:
		requestLogger(r.Context()).Error("project owner operation failed", "err", err.Error())
		writeDetail(w, http.StatusInternalServerError, "project owner service unavailable")
	}
}

func (s *Server) writeProjectOwner(w http.ResponseWriter, r *http.Request, owner store.ProjectOwnership, viewerID string) {
	username := ""
	if owner.OwnerID != "" {
		user, err := s.store.GetUser(r.Context(), owner.OwnerID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			projectOwnerError(w, r, err)
			return
		}
		if err == nil {
			username = user.Username
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"projectId": owner.ProjectID, "viewerUid": viewerID, "revision": owner.Revision, "ownerUid": owner.OwnerID, "ownerUsername": username, "updatedBy": owner.UpdatedBy, "updatedAt": owner.UpdatedAt})
}

func (s *Server) handleProjectOwner(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(r.PathValue("id")))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	owner, err := s.store.ProjectOwnership(r.Context(), r.PathValue("id"))
	if err != nil {
		projectOwnerError(w, r, err)
		return
	}
	s.writeProjectOwner(w, r, owner, user.ID)
}

func (s *Server) handleProjectOwnerUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !s.freshAdmin(r.Context(), user.ID) {
		writeDetail(w, http.StatusForbidden, "administrator access required for project ownership")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	fields, parseErr := proxmoxObject(string(raw))
	var revision int64
	var username string
	if err != nil || !utf8.Valid(raw) || parseErr != nil || len(fields) != 2 || string(fields["revision"]) == "null" || json.Unmarshal(fields["revision"], &revision) != nil || revision < 0 || !syncString(fields["username"], &username) {
		writeDetail(w, http.StatusBadRequest, "expected {revision, username} for explicit project ownership")
		return
	}
	owner, err := s.store.SetProjectOwner(r.Context(), user.ID, r.PathValue("id"), revision, username, func(ctx context.Context) bool { return secretRequestSession(ctx, s, r, user.ID) })
	if err != nil {
		projectOwnerError(w, r, err)
		return
	}
	s.writeProjectOwner(w, r, owner, user.ID)
}
