package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxy"
)

const actorViewTTL = 10 * time.Minute
const actorViewLimit = 4096

type actorViewKey struct{}
type actorView struct {
	readOnly                                                               bool
	managedUID                                                             string
	generation                                                             int64
	token, session, userID, username, swarm, actor, owner, project, origin string
	expires                                                                time.Time
}
type actorViews struct {
	mu      sync.Mutex
	tickets map[string]actorView
}

func (s *Server) actorViewUser(ctx context.Context) (proxy.Identity, bool) {
	if v, ok := ctx.Value(actorViewKey{}).(actorView); ok {
		return proxy.Identity{ID: v.userID, Username: v.username}, true
	}
	return proxyCurrentUser(ctx)
}

// validActorView проверяет исходную сессию, актуальные права и неизменность
// привязки роя. Cookie браузера на sandbox-маршруте не являются полномочием.
func (s *Server) validActorView(v actorView) bool {
	if !time.Now().Before(v.expires) {
		return false
	}
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
	sw, found, err := s.gateway.SwarmByID(v.swarm)
	if err != nil || !found || sw.State != app.StateActive || sw.OwnerID != v.owner || sw.ProjectID != v.project {
		return false
	}
	allowed, err := s.app.Can(id, app.ActionManageSwarm, app.SwarmRes(v.swarm))
	return err == nil && allowed && s.validManagedView(ctx, v)
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

// issueActorView заменяет старый HTTP-прокси переходом в изолированный
// документ. Прямой WS остаётся за прежней проверкой Origin и CSRF.
func (s *Server) issueActorView(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	sw, ok := s.swarmForAction(w, r, user, app.ActionManageSwarm)
	if !ok {
		return
	}
	prefix := "/api/swarms/" + r.PathValue("id") + "/proxy/actors/"
	tail, found := strings.CutPrefix(r.URL.Path, prefix)
	actor, rest, _ := strings.Cut(tail, "/")
	if !found || !viewSegment(actor) || !cleanViewPath(r) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "open actor view first", http.StatusMethodNotAllowed)
		return
	}
	cookie, err := r.Cookie(auth.CookieSession)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		http.Error(w, "cannot create actor view", 500)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	v := actorView{token: token, session: cookie.Value, userID: user.ID, username: user.Username, swarm: sw.ID, actor: actor, owner: sw.OwnerID, project: sw.ProjectID, origin: viewOrigin(r), expires: time.Now().Add(actorViewTTL)}
	uid, generation, err := s.managedViewBinding(r.Context(), sw.ID, actor)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v.managedUID, v.generation = uid, generation
	mode := r.URL.Query().Get("mode")
	if mode != "" && mode != "rw" && mode != "ro" || len(r.URL.Query()["mode"]) > 1 {
		http.Error(w, "terminal mode must be ro or rw", http.StatusBadRequest)
		return
	}
	// Режим выбирает ссылка панели: rw — терминал с вводом (по умолчанию),
	// ro — снимок экрана без ввода. Промежуточной страницы выбора нет.
	v.readOnly = mode == "ro"
	if v.readOnly && uid == "" {
		http.Error(w, "read-only snapshots require a managed actor", http.StatusUnprocessableEntity)
		return
	}
	if !s.validManagedView(r.Context(), v) {
		http.Error(w, "the terminal is not available: the agent is not running or your access changed; reload the agents list", http.StatusConflict)
		return
	}
	s.views.mu.Lock()
	for key, old := range s.views.tickets {
		if !time.Now().Before(old.expires) {
			delete(s.views.tickets, key)
		}
	}
	if len(s.views.tickets) >= actorViewLimit {
		s.views.mu.Unlock()
		http.Error(w, "actor view capacity reached", http.StatusServiceUnavailable)
		return
	}
	s.views.tickets[token] = v
	s.views.mu.Unlock()
	location := "/actor-view/" + token + "/" + url.PathEscape(sw.ID) + "/" + actor + "/" + rest
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// actorViewHandler работает до cookie middleware и ServeMux: Origin:null
// допустим только после проверки capability; грязный путь не редиректится.
func (s *Server) actorViewHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/actor-view/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/actor-view/"), "/", 4)
		if len(parts) != 4 || !cleanViewPath(r) {
			http.NotFound(w, r)
			return
		}
		s.views.mu.Lock()
		v, ok := s.views.tickets[parts[0]]
		s.views.mu.Unlock()
		if !ok || parts[1] != v.swarm || parts[2] != v.actor || viewOrigin(r) != v.origin || !s.validActorView(v) {
			http.NotFound(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && origin != v.origin {
			http.Error(w, "actor view origin denied", http.StatusForbidden)
			return
		}
		if v.readOnly {
			s.serveReadonlyActorView(w, r, v, parts[3])
			return
		}
		clone := r.Clone(context.WithValue(r.Context(), actorViewKey{}, v))
		u := *r.URL
		clone.URL = &u
		clone.URL.Path = "/swarms/" + v.swarm + "/actors/" + v.actor + "/" + parts[3]
		clone.URL.RawPath = ""
		clone.SetPathValue("swarm", v.swarm)
		clone.SetPathValue("actor", v.actor)
		s.proxy.ServeHTTP(w, clone)
	})
}

func (s *Server) actorViewRequest(out, original *http.Request) {
	if v, ok := original.Context().Value(actorViewKey{}).(actorView); ok {
		out.Header.Set("Origin", v.origin)
		out.Header.Del("Cookie")
		out.Header.Del("Referer")
	}
	if _, _, ok := managedGeneration(out.URL.Host); ok {
		out.Host = "127.0.0.1:8080"
		out.Header.Set("Origin", "http://127.0.0.1:8080")
		for _, name := range []string{"Ate-Target-Actor", proxy.HeaderActor, proxy.HeaderAtespace, proxy.HeaderActorTarget} {
			out.Header.Del(name)
		}
	}
}

func (s *Server) actorViewResponse(r *http.Request, response *http.Response) error {
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
		return errors.New("actor redirects are not allowed")
	}
	if v, ok := r.Context().Value(actorViewKey{}).(actorView); ok {
		useDrifttyClient(r, response, v)
		prefix := v.origin + "/actor-view/" + v.token + "/" + v.swarm + "/" + v.actor + "/"
		ws := "ws" + strings.TrimPrefix(prefix, "http")
		h.Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' 'wasm-unsafe-eval' "+prefix+"; style-src 'unsafe-inline' "+prefix+"; connect-src "+prefix+" "+ws+"; img-src "+prefix+" data: blob:; font-src "+prefix+" data:; media-src "+prefix+" blob:; worker-src blob:; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Access-Control-Allow-Origin", "null")
		h.Del("Access-Control-Allow-Credentials")
		h.Del("Access-Control-Allow-Headers")
		h.Del("Access-Control-Allow-Methods")
	}
	return nil
}
