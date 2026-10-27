package auth

import (
	"context"
	"errors"
	"net/http"

	"brood.dev/go/brood/store"
)

func (s *Service) handleAdminRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		IsAdmin *bool `json:"is_admin"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.IsAdmin == nil {
		writeError(w, http.StatusBadRequest, "is_admin is required")
		return
	}
	err := s.store.WithTx(r.Context(), func(ctx context.Context) error {
		current, err := s.store.GetUser(ctx, actor.ID)
		if err != nil {
			return err
		}
		if current.Disabled || !current.IsAdmin {
			return errCredentialsChanged
		}
		target, err := s.store.GetUserByUsername(ctx, r.PathValue("username"))
		if err != nil {
			return err
		}
		if target.IsAdmin == *body.IsAdmin {
			return nil
		}
		if err := s.store.SetUserAdmin(ctx, target.ID, *body.IsAdmin); err != nil {
			return err
		}
		_, err = s.sessions.RevokeAllOfUser(ctx, target.ID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrLastActiveAdmin):
		writeError(w, http.StatusConflict, "cannot remove the last active admin")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, errCredentialsChanged):
		writeError(w, http.StatusForbidden, "administrator access required")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
