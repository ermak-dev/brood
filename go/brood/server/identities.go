package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"brood.dev/go/brood/substrate"
)

type nativeIdentity struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func (s *Server) handleNativeIdentities(w http.ResponseWriter, r *http.Request) {
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
	policy, err := client.GetGlobalAccessPolicy(r.Context())
	if err != nil {
		var apiErr *substrate.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			apiError(w, r, err)
			return
		}
	}
	// Это объявленные инфраструктурные ServiceAccount установки (docs/SUBSTRATE.md),
	// не преобразование локальных пользователей в несуществующие native identity.
	known := map[string]bool{
		"user:system:serviceaccount:brood:django":          true,
		"user:system:serviceaccount:ate-system:ate-client": true,
	}
	for _, binding := range policy.GetBindings() {
		for _, member := range binding.GetMembers() {
			if strings.HasPrefix(member, "user:") && member != "user:" && member != "user:*" {
				known[member] = true
			}
		}
	}
	result := make([]nativeIdentity, 0, len(known))
	for id := range known {
		result = append(result, nativeIdentity{ID: id, Label: strings.TrimPrefix(id, "user:")})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	writeJSON(w, http.StatusOK, result)
}
