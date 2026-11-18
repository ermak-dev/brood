package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

// handleChildAbort is deliberately separate from guest scheduling and swarm stop.
// The current owner/admin explicitly confirms recovery at the exact revision.
func (s *Server) handleChildAbort(w http.ResponseWriter, r *http.Request) {
	user, ok := s.visibleSwarmUser(w, r)
	if !ok {
		return
	}
	sequence, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
	var in struct {
		Revision int64 `json:"revision"`
		Confirm  bool  `json:"confirm"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err != nil || sequence < 1 || dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF || in.Revision < 1 || !in.Confirm {
		writeDetail(w, http.StatusBadRequest, "child recovery requires an exact revision and confirm=true")
		return
	}
	cookie, err := r.Cookie(auth.CookieSession)
	if err != nil || !secretRequestSession(r.Context(), s, r, user.ID) {
		writeDetail(w, http.StatusForbidden, "child recovery requires an active owner/admin web session and explicit confirmation")
		return
	}
	p, err := s.store.SwarmChildParentAt(r.Context(), r.PathValue("id"), sequence)
	if err != nil {
		writeDetail(w, http.StatusNotFound, "child group not found")
		return
	}
	policy := func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if a.HumanID != user.ID || !secretRequestSession(ctx, s, r, user.ID) {
			return store.ErrSwarmChildDenied
		}
		return nil
	}
	sum := sha256.Sum256([]byte(cookie.Value))
	j, err := s.store.AbortSwarmChildJoin(r.Context(), p, fmt.Sprintf("%x", sum), in.Revision, policy)
	switch {
	case errors.Is(err, store.ErrSwarmChildDenied):
		writeDetail(w, http.StatusForbidden, "child recovery requires an active owner/admin web session and explicit confirmation")
	case errors.Is(err, store.ErrSwarmChildConflict):
		writeDetail(w, http.StatusConflict, "only incomplete groups with no running or completed child can be aborted at the exact revision")
	case err != nil:
		writeDetail(w, http.StatusInternalServerError, "child recovery unavailable")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"state": j.State, "revision": j.Revision, "open": j.Evaluation.Open})
	}
}
