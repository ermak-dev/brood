package server

import (
	"errors"
	"net/http"

	"brood.dev/go/brood/store"
)

func (s *Server) handleMemberList(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetProject(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	} else if err != nil {
		apiError(w, r, err)
		return
	}
	members, err := s.store.MembersOf(r.Context(), id)
	if err != nil {
		apiError(w, r, err)
		return
	}
	type member struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		IsAdmin     bool   `json:"is_admin"`
		Disabled    bool   `json:"disabled"`
	}
	out := make([]member, 0, len(members))
	for _, user := range members {
		displayName := ""
		if user.DisplayName != nil {
			displayName = *user.DisplayName
		}
		out = append(out, member{user.Username, displayName, user.IsAdmin, user.Disabled})
	}
	writeJSON(w, http.StatusOK, out)
}
