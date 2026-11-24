package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxy"
)

const agentViewTTL = 10 * time.Minute
const agentViewLimit = 4096

// agentViewPrefix — маршрут пропуска терминала:
// /agent-view/{token}/{swarm}/{agent}/[путь терминала].
const agentViewPrefix = "/agent-view/"

type agentViewKey struct{}
type agentView struct {
	readOnly                                                               bool
	uid                                                                    string
	generation                                                             int64
	token, session, userID, username, swarm, agent, owner, project, origin string
	expires                                                                time.Time
}

// path — корень терминала под пропуском.
func (v agentView) path() string {
	return agentViewPrefix + v.token + "/" + v.swarm + "/" + v.agent + "/"
}

// agentViews — выданные пропуска; нулевое значение готово к работе.
type agentViews struct {
	mu      sync.Mutex
	tickets map[string]agentView
}

func (s *Server) agentViewUser(ctx context.Context) (proxy.Identity, bool) {
	if v, ok := ctx.Value(agentViewKey{}).(agentView); ok {
		return proxy.Identity{ID: v.userID, Username: v.username}, true
	}
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return proxy.Identity{}, false
	}
	return proxy.Identity{ID: user.ID, Username: user.Username}, true
}

func (s *Server) validAgentView(v agentView) bool {
	return time.Now().Before(v.expires) && s.validAgentViewAccess(v)
}

// Cookie браузера на sandbox-маршруте не заменяет исходную сессию.
func (s *Server) validAgentViewAccess(v agentView) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, _, err := auth.NewSessions(s.store, 0).Load(ctx, v.session, time.Now())
	if err != nil || id != v.userID {
		return false
	}
	user, err := s.store.GetUser(ctx, id)
	if err != nil || user.Disabled {
		return false
	}
	sw, found, err := s.app.SwarmByID(ctx, v.swarm)
	if err != nil || !found || sw.State != app.StateActive || sw.OwnerID != v.owner || sw.ProjectID != v.project {
		return false
	}
	allowed, err := s.app.Can(ctx, id, app.ActionManageSwarm, app.SwarmRes(v.swarm))
	return err == nil && allowed && s.validAgentViewBinding(ctx, v)
}

func viewSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func cleanViewPath(r *http.Request) bool {
	if strings.ContainsAny(r.URL.EscapedPath(), "%\\") || strings.Contains(r.URL.Path, "//") {
		return false
	}
	for _, part := range strings.Split(r.URL.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func viewOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// handleAgentTerminal — /api/swarms/{id}/proxy/agents/{name}/[путь]. Обычный
// запрос получает пропуск и уходит на /agent-view/; WebSocket с cookie
// сессии идёт к прокси напрямую, за проверкой Origin и CSRF.
func (s *Server) handleAgentTerminal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !viewSegment(name) || !cleanViewPath(r) {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Upgrade") == "" {
		s.issueAgentView(w, r, name, r.PathValue("path"))
		return
	}
	s.proxy.Serve(w, r, r.PathValue("id"), name, "/"+r.PathValue("path"))
}

// issueAgentView выдаёт пропуск терминала и уводит в изолированный
// документ под ним.
func (s *Server) issueAgentView(w http.ResponseWriter, r *http.Request, name, rest string) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	sw, ok := s.swarmForAction(w, r, user, app.ActionManageSwarm)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "open the terminal from the agents list first", http.StatusMethodNotAllowed)
		return
	}
	cookie, err := r.Cookie(auth.CookieSession)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		http.Error(w, "cannot open the terminal", 500)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	v := agentView{token: token, session: cookie.Value, userID: user.ID, username: user.Username, swarm: sw.ID, agent: name, owner: sw.OwnerID, project: sw.ProjectID, origin: viewOrigin(r), expires: time.Now().Add(agentViewTTL)}
	v.uid, v.generation, err = s.agentViewBinding(r.Context(), sw.ID, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "" && mode != "rw" && mode != "ro" || len(r.URL.Query()["mode"]) > 1 {
		http.Error(w, "terminal mode must be ro or rw", http.StatusBadRequest)
		return
	}
	// Режим выбирает ссылка панели: rw — терминал с вводом (по умолчанию),
	// ro — снимок экрана без ввода. Промежуточной страницы выбора нет.
	v.readOnly = mode == "ro"
	if !s.validAgentViewBinding(r.Context(), v) {
		http.Error(w, "the terminal is not available: the agent is not running or your access changed; reload the agents list", http.StatusConflict)
		return
	}
	s.views.mu.Lock()
	for key, old := range s.views.tickets {
		if !time.Now().Before(old.expires) {
			delete(s.views.tickets, key)
		}
	}
	if len(s.views.tickets) >= agentViewLimit {
		s.views.mu.Unlock()
		http.Error(w, "too many open terminals; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	if s.views.tickets == nil {
		s.views.tickets = make(map[string]agentView)
	}
	s.views.tickets[token] = v
	s.views.mu.Unlock()
	location := v.path() + rest
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// agentViewHandler работает до cookie middleware и ServeMux: Origin:null
// допустим только после проверки capability; грязный путь не редиректится.
func (s *Server) agentViewHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, agentViewPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, agentViewPrefix), "/", 4)
		if len(parts) != 4 || !cleanViewPath(r) {
			http.NotFound(w, r)
			return
		}
		s.views.mu.Lock()
		v, ok := s.views.tickets[parts[0]]
		s.views.mu.Unlock()
		if !ok || parts[1] != v.swarm || parts[2] != v.agent || viewOrigin(r) != v.origin || !s.validAgentView(v) {
			http.NotFound(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && origin != v.origin {
			http.Error(w, "terminal origin denied", http.StatusForbidden)
			return
		}
		if v.readOnly {
			s.serveReadonlyAgentView(w, r, v, parts[3])
			return
		}
		s.proxy.Serve(w, r.WithContext(context.WithValue(r.Context(), agentViewKey{}, v)), v.swarm, v.agent, "/"+parts[3])
	})
}

// agentViewRequest не пускает к агенту cookie и Referer страницы пропуска.
func (s *Server) agentViewRequest(out, original *http.Request) {
	if _, ok := original.Context().Value(agentViewKey{}).(agentView); ok {
		out.Header.Del("Cookie")
		out.Header.Del("Referer")
	}
}

func (s *Server) agentViewResponse(r *http.Request, response *http.Response) error {
	h := response.Header
	// Sandbox изолирует DOM, но не побочные эффекты HTTP для origin панели.
	for _, name := range []string{"Set-Cookie", "Set-Cookie2", "Clear-Site-Data", "Alt-Svc", "Strict-Transport-Security", "WWW-Authenticate", "Authentication-Info", "Refresh", "Link", "Report-To", "Reporting-Endpoints", "NEL", "Origin-Trial", "Service-Worker-Allowed", "Content-Security-Policy-Report-Only"} {
		h.Del(name)
	}
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return errors.New("agent terminal redirects are not allowed")
	}
	if v, ok := r.Context().Value(agentViewKey{}).(agentView); ok {
		useDrifttyClient(r, response, v)
		prefix := v.origin + v.path()
		ws := "ws" + strings.TrimPrefix(prefix, "http")
		h.Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' 'wasm-unsafe-eval' "+prefix+"; style-src 'unsafe-inline' "+prefix+"; connect-src "+prefix+" "+ws+"; img-src "+prefix+" data: blob:; font-src "+prefix+" data:; media-src "+prefix+" blob:; worker-src blob:; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Access-Control-Allow-Origin", "null")
		h.Del("Access-Control-Allow-Credentials")
		h.Del("Access-Control-Allow-Headers")
		h.Del("Access-Control-Allow-Methods")
	}
	return nil
}
