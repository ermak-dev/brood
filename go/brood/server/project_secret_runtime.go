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
	"brood.dev/go/brood/store"
)

const projectSecretRuntimePrefix = "/api/proxmox/secrets/"

var projectSecretActorUID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func publicProjectSecretPath(path string) bool {
	return strings.HasPrefix(path, projectSecretRuntimePrefix)
}

// guestRuntime — проверенный машинный запрос root supervisor гостя к
// <prefix><uid>/<generation>: актор, его рой и единственный guest token.
type guestRuntime struct {
	uid        string
	generation int64
	token      string
	actor      store.ManagedActor
	swarm      store.Swarm
}

// openGuestRuntime — общая авторизация машинных маршрутов гостя вне
// cookie/CSRF middleware: только GET точного пути без query и тела, ровно один
// Bearer token этого UID и поколения при текущем start intent, активная
// привязка роя и namespace. Отказ уже записан в w, когда ok == false;
// unavailable — деталь 503 маршрута.
func (s *Server) openGuestRuntime(ctx context.Context, w http.ResponseWriter, r *http.Request, prefix, unavailable string) (guestRuntime, bool) {
	var g guestRuntime
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeDetail(w, 405, "method not allowed")
		return g, false
	}
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 {
		writeDetail(w, 404, "not found")
		return g, false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 2 || !projectSecretActorUID.MatchString(parts[0]) {
		writeDetail(w, 404, "not found")
		return g, false
	}
	generation, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != parts[1] {
		writeDetail(w, 404, "not found")
		return g, false
	}
	credentials := r.Header.Values("Authorization")
	token := ""
	if len(credentials) == 1 && strings.HasPrefix(credentials[0], "Bearer ") {
		token = strings.TrimPrefix(credentials[0], "Bearer ")
	}
	uid := parts[0]
	if s.managed == nil || s.managed.hub == nil || s.managed.registry == nil || len(s.managed.registry.relaySecret) < 32 || len(token) != 43 || !s.managed.authorizeGuest(uid, generation, token) {
		writeDetail(w, 401, "guest authorization denied")
		return g, false
	}
	actor, err := s.store.GetManagedActor(ctx, uid)
	if err != nil {
		writeDetail(w, 401, "guest authorization denied")
		return g, false
	}
	swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil || swarm.State != "active" || swarm.AtespaceUID == nil || *swarm.AtespaceUID != actor.AtespaceUID {
		writeDetail(w, 401, "guest authorization denied")
		return g, false
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		writeDetail(w, 503, unavailable)
		return g, false
	}
	if !s.managed.authorizeGuest(uid, generation, token) {
		writeDetail(w, 401, "guest authorization denied")
		return g, false
	}
	return guestRuntime{uid: uid, generation: generation, token: token, actor: actor, swarm: swarm}, true
}

// guestRuntimeCurrent повторяет привязку после чтения ответа: тот же проект, активный рой
// того же atespace и всё ещё действующий token.
func (s *Server) guestRuntimeCurrent(ctx context.Context, g guestRuntime) bool {
	current, err := s.store.GetSwarm(ctx, g.actor.SwarmID)
	return err == nil && current.ProjectID == g.swarm.ProjectID && current.State == "active" && current.AtespaceUID != nil && *current.AtespaceUID == g.actor.AtespaceUID && s.managed.authorizeGuest(g.uid, g.generation, g.token)
}

// writeGuestRuntime отдаёт готовый JSON с ограниченным сроком записи.
func writeGuestRuntime(w http.ResponseWriter, data []byte) {
	control := http.NewResponseController(w)
	_ = control.SetWriteDeadline(time.Now().Add(15 * time.Second))
	defer control.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) serveGuestProjectSecrets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	g, ok := s.openGuestRuntime(ctx, w, r, projectSecretRuntimePrefix, "project secret delivery unavailable")
	if !ok {
		return
	}
	bundle, err := s.store.ReadProjectSecretBundle(ctx, g.swarm.ProjectID)
	if err != nil || projectsecret.ValidateBundle(bundle) != nil {
		writeDetail(w, 503, "project secret delivery unavailable")
		return
	}
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > projectSecretBodyLimit {
		writeDetail(w, 503, "project secret delivery exceeds limits")
		return
	}
	if !s.guestRuntimeCurrent(ctx, g) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	writeGuestRuntime(w, data)
}
