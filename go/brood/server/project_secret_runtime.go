package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/store"
)

const projectSecretRuntimePrefix = "/api/proxmox/secrets/"
const guestRolePromptPrefix = "/api/proxmox/role-prompt/"

func publicGuestRolePromptPath(path string) bool {
	return strings.HasPrefix(path, guestRolePromptPrefix)
}

func (s *Server) serveGuestRolePrompt(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	const unavailable = "Agent role prompt is unavailable; check the assigned role and retry starting the session"
	g, _, ok := s.openGuestRuntime(ctx, w, r, guestRolePromptPrefix, unavailable, noGuestRuntimeRest)
	if !ok {
		return
	}
	role, err := s.store.GetAgentRole(ctx, g.actor.Role)
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, unavailable)
		return
	}
	if !s.guestRuntimeCurrent(ctx, g) {
		writeDetail(w, http.StatusUnauthorized, "guest authorization denied")
		return
	}
	control := http.NewResponseController(w)
	_ = control.SetWriteDeadline(time.Now().Add(15 * time.Second))
	defer control.SetWriteDeadline(time.Time{})
	if role.Model != "" {
		w.Header().Set(actormodel.RoleModelHeader, role.Model)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(role.Prompt))
}

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

// noGuestRuntimeRest — маршруты без хвоста после <uid>/<generation>.
func noGuestRuntimeRest(rest []string) bool { return len(rest) == 0 }

// openGuestRuntime — общая авторизация машинных маршрутов гостя вне
// cookie/CSRF middleware: только GET, без query и тела, ровно один Bearer
// token этого UID и поколения при текущем start intent, активная привязка
// роя и namespace. restOK решает судьбу хвоста пути после <uid>/<generation>
// до авторизации: у секретов и workspace хвоста нет, у артефактов — имя
// файла из закрытого списка. Отказ уже записан в w, когда ok == false;
// unavailable — деталь 503 маршрута. Второе значение — принятый хвост.
func (s *Server) openGuestRuntime(ctx context.Context, w http.ResponseWriter, r *http.Request, prefix, unavailable string, restOK func([]string) bool) (guestRuntime, []string, bool) {
	var g guestRuntime
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeDetail(w, 405, "method not allowed")
		return g, nil, false
	}
	if r.ContentLength != 0 {
		writeDetail(w, 404, "not found")
		return g, nil, false
	}
	return s.authorizeGuestRuntime(ctx, w, r, prefix, restOK)
}

func (s *Server) authorizeGuestRuntime(ctx context.Context, w http.ResponseWriter, r *http.Request, prefix string, restOK func([]string) bool) (guestRuntime, []string, bool) {
	var g guestRuntime
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || !strings.HasPrefix(r.URL.Path, prefix) {
		writeDetail(w, 404, "not found")
		return g, nil, false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	var rest []string
	if len(parts) > 2 {
		rest = parts[2:]
	}
	if len(parts) < 2 || !projectSecretActorUID.MatchString(parts[0]) || !restOK(rest) {
		writeDetail(w, 404, "not found")
		return g, nil, false
	}
	generation, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != parts[1] {
		writeDetail(w, 404, "not found")
		return g, nil, false
	}
	credentials := r.Header.Values("Authorization")
	token := ""
	if len(credentials) == 1 && strings.HasPrefix(credentials[0], "Bearer ") {
		token = strings.TrimPrefix(credentials[0], "Bearer ")
	}
	uid := parts[0]
	if s.managed == nil || s.managed.hub == nil || s.managed.registry == nil || len(s.managed.registry.relaySecret) < 32 || len(token) != 43 || !s.managed.authorizeGuest(uid, generation, token) {
		writeDetail(w, 401, "guest authorization denied")
		return g, nil, false
	}
	actor, err := s.store.GetManagedActor(ctx, uid)
	if err != nil {
		writeDetail(w, 401, "guest authorization denied")
		return g, nil, false
	}
	swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil || swarm.State != "active" {
		writeDetail(w, 401, "guest authorization denied")
		return g, nil, false
	}
	if !s.managed.authorizeGuest(uid, generation, token) {
		writeDetail(w, 401, "guest authorization denied")
		return g, nil, false
	}
	return guestRuntime{uid: uid, generation: generation, token: token, actor: actor, swarm: swarm}, rest, true
}

// guestRuntimeCurrent повторяет привязку после чтения ответа: тот же проект, активный рой
// того же atespace и всё ещё действующий token.
func (s *Server) guestRuntimeCurrent(ctx context.Context, g guestRuntime) bool {
	current, err := s.store.GetSwarm(ctx, g.actor.SwarmID)
	return err == nil && current.ProjectID == g.swarm.ProjectID && current.State == "active" && s.managed.authorizeGuest(g.uid, g.generation, g.token)
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
	g, _, ok := s.openGuestRuntime(ctx, w, r, projectSecretRuntimePrefix, "project secret delivery unavailable", noGuestRuntimeRest)
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
