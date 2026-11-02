package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"brood.dev/go/brood/projectsecret"
)

const projectSecretRuntimePrefix = "/api/proxmox/secrets/"

var projectSecretActorUID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func publicProjectSecretPath(path string) bool {
	return strings.HasPrefix(path, projectSecretRuntimePrefix)
}

func (s *Server) serveGuestProjectSecrets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeDetail(w, 405, "method not allowed")
		return
	}
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 {
		writeDetail(w, 404, "not found")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, projectSecretRuntimePrefix), "/")
	if len(parts) != 2 || !projectSecretActorUID.MatchString(parts[0]) {
		writeDetail(w, 404, "not found")
		return
	}
	generation, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != parts[1] {
		writeDetail(w, 404, "not found")
		return
	}
	credentials := r.Header.Values("Authorization")
	token := ""
	if len(credentials) == 1 && strings.HasPrefix(credentials[0], "Bearer ") {
		token = strings.TrimPrefix(credentials[0], "Bearer ")
	}
	uid := parts[0]
	if s.managed == nil || s.managed.hub == nil || len(s.managed.registry.relaySecret) < 32 || len(token) != 43 || !s.managed.authorizeGuest(uid, generation, token) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	actor, err := s.store.GetManagedActor(ctx, uid)
	if err != nil {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil || swarm.State != "active" || swarm.AtespaceUID == nil || *swarm.AtespaceUID != actor.AtespaceUID {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		writeDetail(w, 503, "project secret delivery unavailable")
		return
	}
	if !s.managed.authorizeGuest(uid, generation, token) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	bundle, err := s.store.ReadProjectSecretBundle(ctx, swarm.ProjectID)
	if err != nil || projectsecret.ValidateBundle(bundle) != nil {
		writeDetail(w, 503, "project secret delivery unavailable")
		return
	}
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > projectSecretBodyLimit {
		writeDetail(w, 503, "project secret delivery exceeds limits")
		return
	}
	current, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil || current.ProjectID != swarm.ProjectID || current.State != "active" || current.AtespaceUID == nil || *current.AtespaceUID != actor.AtespaceUID || !s.managed.authorizeGuest(uid, generation, token) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	control := http.NewResponseController(w)
	_ = control.SetWriteDeadline(time.Now().Add(15 * time.Second))
	defer control.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
