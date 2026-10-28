package auth

import (
	"context"
	"errors"
	"net/http"

	"brood.dev/go/brood/store"
)

func (s *Service) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	username := r.PathValue("username")
	if r.Method == http.MethodGet {
		target, err := s.store.GetUserByUsername(r.Context(), username)
		if err != nil {
			s.writeUserMutationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			userInfo
			Disabled bool `json:"disabled"`
		}{userInfo{Username: target.Username, DisplayName: strOrEmpty(target.DisplayName), IsAdmin: target.IsAdmin}, target.Disabled})
		return
	}
	var body struct {
		DisplayName *string `json:"display_name"`
		IsAdmin     *bool   `json:"is_admin"`
		Disabled    *bool   `json:"disabled"`
	}
	if r.Method == http.MethodPut {
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.DisplayName == nil || body.IsAdmin == nil || body.Disabled == nil {
			writeError(w, http.StatusBadRequest, "display_name, is_admin and disabled are required")
			return
		}
	}
	err := s.store.WithTx(r.Context(), func(ctx context.Context) error {
		current, err := s.store.GetUser(ctx, actor.ID)
		if err != nil {
			return err
		}
		if current.Disabled || !current.IsAdmin {
			return errCredentialsChanged
		}
		target, err := s.store.GetUserByUsername(ctx, username)
		if err != nil {
			return err
		}
		if r.Method == http.MethodDelete {
			return s.store.DeleteUserWithoutSwarms(ctx, target.ID)
		}
		return s.store.UpdateUserProperties(ctx, target.ID, *body.DisplayName, *body.IsAdmin, *body.Disabled)
	})
	if err != nil {
		s.writeUserMutationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeUserMutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrLastActiveAdmin):
		writeError(w, http.StatusConflict, "cannot remove the last active admin")
	case errors.Is(err, store.ErrUserOwnsSwarms):
		writeError(w, http.StatusConflict, "user still owns swarms; transfer or delete them first")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, errCredentialsChanged):
		writeError(w, http.StatusForbidden, "administrator access required")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
