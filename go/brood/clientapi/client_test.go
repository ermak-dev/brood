package clientapi

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAuthServer — httptest-сервер, реализующий контракт входа из CONTRACT.md:
// bootstrap CSRF для анонимных запросов, login с проверкой X-CSRFToken,
// ротация csrf, logout с удалением сессии и защищённые whoami/projects.
type fakeAuthServer struct {
	*httptest.Server

	mu       sync.Mutex
	sessions map[string]string // session token -> username
	nextID   int
	// redirectOnLogin: если не пусто, login отвечает 302 на этот адрес.
	redirectOnLogin string
}

// handler собирает mux контрактного API; f должен быть создан заранее.
func (f *fakeAuthServer) handler() http.Handler {
	f.mu.Lock()
	f.sessions = map[string]string{}
	f.mu.Unlock()
	mux := http.NewServeMux()

	issueCSRF := func(w http.ResponseWriter, r *http.Request) string {
		f.mu.Lock()
		f.nextID++
		token := fmt.Sprintf("csrf-%d", f.nextID)
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "csrf", Value: token, Path: "/"})
		return token
	}

	checkCSRF := func(w http.ResponseWriter, r *http.Request) bool {
		c, err := r.Cookie("csrf")
		if err != nil || c.Value == "" || r.Header.Get("X-CSRFToken") != c.Value {
			writeDetail(w, http.StatusForbidden, "CSRF check failed")
			return false
		}
		return true
	}

	currentUser := func(r *http.Request) (string, bool) {
		c, err := r.Cookie("session")
		if err != nil {
			return "", false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		name, ok := f.sessions[c.Value]
		return name, ok
	}

	sessionJSON := func(name string) map[string]any {
		return map[string]any{
			"authenticated": true,
			"user": map[string]any{
				"username":     name,
				"display_name": strings.ToUpper(name),
				"is_admin":     name == "admin",
			},
		}
	}

	mux.HandleFunc("GET /api/auth/session", func(w http.ResponseWriter, r *http.Request) {
		if name, ok := currentUser(r); ok {
			writeJSON(w, http.StatusOK, sessionJSON(name))
			return
		}
		issueCSRF(w, r)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
	})

	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if !checkCSRF(w, r) {
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeDetail(w, http.StatusBadRequest, "invalid request body")
			return
		}
		f.mu.Lock()
		target := f.redirectOnLogin
		f.mu.Unlock()
		if target != "" {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		if body.Username != "alice" || body.Password != "pass123" {
			writeDetail(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		f.mu.Lock()
		f.nextID++
		token := fmt.Sprintf("sess-%d", f.nextID)
		f.sessions[token] = body.Username
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/"})
		issueCSRF(w, r) // ротация csrf при входе
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if !checkCSRF(w, r) {
			return
		}
		c, err := r.Cookie("session")
		if err != nil {
			writeDetail(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		f.mu.Lock()
		_, ok := f.sessions[c.Value]
		delete(f.sessions, c.Value)
		f.mu.Unlock()
		if !ok {
			writeDetail(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1})
		http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "", Path: "/", MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		name, ok := currentUser(r)
		if !ok {
			writeDetail(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		writeJSON(w, http.StatusOK, []map[string]string{
			{"id": "p1", "name": "alpha-" + name},
			{"id": "p2", "name": "beta"},
		})
	})
	return mux
}

// newFakeAuthServer поднимает контрактный сервер аутентификации.
func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	f := &fakeAuthServer{sessions: map[string]string{}}
	f.Server = httptest.NewServer(f.handler())
	t.Cleanup(f.Close)
	return f
}

// newFakeAuthServerTLS — то же, но с self-signed TLS и доступным сертификатом.
func newFakeAuthServerTLS(t *testing.T) *fakeAuthServer {
	t.Helper()
	f := &fakeAuthServer{sessions: map[string]string{}}
	f.Server = httptest.NewTLSServer(f.handler())
	t.Cleanup(f.Close)
	return f
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeDetail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"detail": msg})
}

// pemOf кодирует DER-сертификат в PEM для WithCACert.
func pemOf(t *testing.T, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestSessionAnonymousBootstrapsCSRF(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := c.Session(context.Background())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if info.Authenticated {
		t.Fatal("anonymous session reported as authenticated")
	}
	if c.Stored().CSRF == "" {
		t.Fatal("csrf cookie not captured from bootstrap response")
	}
}

func TestLoginFullFlow(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := c.Login(context.Background(), "alice", "pass123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !info.Authenticated || info.User == nil || info.User.Username != "alice" {
		t.Fatalf("unexpected login result: %+v", info)
	}
	stored := c.Stored()
	if stored.Session == "" || stored.CSRF == "" {
		t.Fatalf("session/csrf cookies not stored after login: %+v", stored)
	}
	// После входа Session() ходит с теми же cookie и не теряет их.
	again, err := c.Session(context.Background())
	if err != nil || !again.Authenticated {
		t.Fatalf("Session after login: %+v, %v", again, err)
	}
	if c.Stored() != stored {
		t.Fatalf("cookies changed by read-only call: %+v", c.Stored())
	}
}

func TestLoginInvalidCredentials(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, _ := New(srv.URL)
	_, err := c.Login(context.Background(), "alice", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestWhoamiSessionExpired(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, _ := New(srv.URL)
	c.Restore(StoredSession{Session: "sess-dead", CSRF: "csrf-dead"})
	_, err := c.Whoami(context.Background())
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("want ErrSessionExpired, got %v", err)
	}
}

func TestLogoutRevokesAndClears(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, _ := New(srv.URL)
	if _, err := c.Login(context.Background(), "alice", "pass123"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	token := c.Stored().Session
	if err := c.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if c.Stored() != (StoredSession{}) {
		t.Fatalf("cookies not cleared after logout: %+v", c.Stored())
	}
	srv.mu.Lock()
	_, alive := srv.sessions[token]
	srv.mu.Unlock()
	if alive {
		t.Fatal("server still holds the session after logout")
	}
}

func TestLogoutNetworkErrorKeepsCookies(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, _ := New(srv.URL)
	if _, err := c.Login(context.Background(), "alice", "pass123"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	srv.CloseClientConnections()
	srv.Close()
	err := c.Logout(context.Background())
	if err == nil {
		t.Fatal("network failure reported as successful logout")
	}
	if c.Stored() == (StoredSession{}) {
		t.Fatal("cookies dropped despite failed server revoke")
	}
}

func TestProjectsList(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, _ := New(srv.URL)
	if _, err := c.Login(context.Background(), "alice", "pass123"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	projects, err := c.ProjectsList(context.Background())
	if err != nil {
		t.Fatalf("ProjectsList: %v", err)
	}
	if len(projects) != 2 || projects[0].ID != "p1" || projects[0].Name != "alpha-alice" {
		t.Fatalf("unexpected projects: %+v", projects)
	}
}

func TestProjectsListUnauthenticated(t *testing.T) {
	srv := newFakeAuthServer(t)
	c, _ := New(srv.URL)
	c.Restore(StoredSession{Session: "sess-dead", CSRF: "csrf-dead"})
	_, err := c.ProjectsList(context.Background())
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("want ErrSessionExpired, got %v", err)
	}
}

func TestLoginRefusesCrossOriginRedirect(t *testing.T) {
	srv := newFakeAuthServer(t)
	srv.mu.Lock()
	srv.redirectOnLogin = "https://evil.example/catch"
	srv.mu.Unlock()
	c, _ := New(srv.URL)
	_, err := c.Login(context.Background(), "alice", "pass123")
	if err == nil {
		t.Fatal("login followed a redirect without refusal")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error does not mention redirect: %v", err)
	}
	if strings.Contains(err.Error(), "pass123") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestGetRefusesCrossOriginRedirect(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached evil origin: %s", r.URL)
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	_, err := c.Session(context.Background())
	if err == nil {
		t.Fatal("cross-origin redirect followed silently")
	}
}

func TestTLSDefaultVerificationOn(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the server despite broken TLS trust")
	}))
	defer srv.Close()
	// Без доверенного CA соединение обязано падать: InsecureSkipVerify нет.
	c, _ := New(srv.URL)
	if _, err := c.Session(context.Background()); err == nil {
		t.Fatal("TLS verification skipped without a trusted CA")
	}
}

func TestTLSWithCustomCA(t *testing.T) {
	srv := newFakeAuthServerTLS(t)
	c, err := New(srv.URL, WithCACert(pemOf(t, srv.Certificate().Raw)))
	if err != nil {
		t.Fatalf("New with CA: %v", err)
	}
	info, err := c.Login(context.Background(), "alice", "pass123")
	if err != nil {
		t.Fatalf("Login over TLS: %v", err)
	}
	if !info.Authenticated {
		t.Fatal("not authenticated after TLS login")
	}
}

func TestNormalizeOrigin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://brood.example", "https://brood.example"},
		{"brood.example", "https://brood.example"},
		{"brood.example:8443", "https://brood.example:8443"},
		{"http://localhost:8000/", "http://localhost:8000"},
		{"  https://brood.example/  ", "https://brood.example"},
	}
	for _, tc := range cases {
		got, err := NormalizeOrigin(tc.in)
		if err != nil {
			t.Fatalf("NormalizeOrigin(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "https://", "://x", "https://brood.example/pa th"} {
		if _, err := NormalizeOrigin(bad); err == nil {
			t.Fatalf("NormalizeOrigin(%q) unexpectedly succeeded", bad)
		}
	}
}
