package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

// testStore — хранилище во временном файле; закрытие и уборка — в t.Cleanup.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite3")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// testServer — сервис аутентификации с быстрым Argon2 и httptest-сервер.
// Возвращает сервер и токен cookie-клиента (значения session и csrf).
type testServer struct {
	t       *testing.T
	svc     *Service
	server  *httptest.Server
	client  *http.Client
	session string
	csrf    string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	st := testStore(t)
	ts := &testServer{t: t}
	ts.svc = NewService(st, Config{Argon2: fastArgon2()}, nil)
	mux := http.NewServeMux()
	ts.svc.Mount(mux)
	ts.server = httptest.NewServer(ts.svc.Middleware(mux))
	t.Cleanup(ts.server.Close)
	// Cookie обрабатываем явно, как клиентapi: значения живут здесь.
	ts.client = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return ts
}

// do выполняет запрос с текущей парой session/csrf и обновляет её из
// Set-Cookie.
func (ts *testServer) do(method, path, body string, hdr map[string]string) *http.Response {
	ts.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.server.URL+path, rdr)
	if err != nil {
		ts.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", ts.server.URL)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if ts.session != "" {
		req.AddCookie(&http.Cookie{Name: CookieSession, Value: ts.session})
	}
	if ts.csrf != "" {
		req.AddCookie(&http.Cookie{Name: CookieCSRF, Value: ts.csrf})
		if method != http.MethodGet && method != http.MethodHead {
			req.Header.Set(HeaderCSRF, ts.csrf)
		}
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		ts.t.Fatalf("do %s %s: %v", method, path, err)
	}
	ts.absorb(resp)
	return resp
}

// absorb переносит Set-Cookie в состояние клиента.
func (ts *testServer) absorb(resp *http.Response) {
	for _, ck := range resp.Cookies() {
		deleted := ck.Value == "" || ck.MaxAge < 0
		switch ck.Name {
		case CookieSession:
			if deleted {
				ts.session = ""
			} else if ck.Value != "" {
				ts.session = ck.Value
			}
		case CookieCSRF:
			if deleted {
				ts.csrf = ""
			} else if ck.Value != "" {
				ts.csrf = ck.Value
			}
		}
	}
}

// bootstrap — анонимный GET /api/auth/session: получаем csrf cookie.
func (ts *testServer) bootstrap() {
	ts.t.Helper()
	resp := ts.do(http.MethodGet, "/api/auth/session", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ts.t.Fatalf("bootstrap: status %d", resp.StatusCode)
	}
	if ts.csrf == "" {
		ts.t.Fatal("bootstrap: no csrf cookie")
	}
}

// addUser — прямой пользователь в хранилище (минуя admin API).
func (ts *testServer) addUser(username, password string, admin bool) store.User {
	ts.t.Helper()
	hash, err := HashPassword(password, fastArgon2())
	if err != nil {
		ts.t.Fatalf("hash: %v", err)
	}
	u := store.User{ID: store.NewID(), Username: username, PasswordHash: hash, IsAdmin: admin}
	if err := ts.svc.store.CreateUser(context.Background(), u); err != nil {
		ts.t.Fatalf("create user: %v", err)
	}
	return u
}

// login — полный поток входа: bootstrap и POST login.
func (ts *testServer) login(username, password string) int {
	ts.t.Helper()
	ts.bootstrap()
	resp := ts.do(http.MethodPost, "/api/auth/login",
		`{"username":"`+username+`","password":"`+password+`"}`, nil)
	resp.Body.Close()
	return resp.StatusCode
}

func body(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func TestAnonymousSessionBootstrap(t *testing.T) {
	ts := newTestServer(t)
	resp := ts.do(http.MethodGet, "/api/auth/session", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	m := body(t, resp)
	if m["authenticated"] != false {
		t.Fatalf("want authenticated=false, got %v", m["authenticated"])
	}
	// csrf cookie выдана анониму и не HttpOnly.
	var csrfCookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == CookieCSRF {
			csrfCookie = ck
		}
	}
	if csrfCookie == nil || csrfCookie.Value == "" {
		t.Fatal("anonymous GET session must set csrf cookie")
	}
	if csrfCookie.HttpOnly {
		t.Fatal("csrf cookie must be readable by JS (not HttpOnly)")
	}
	if csrfCookie.Path != "/" || csrfCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("csrf cookie attributes: %+v", csrfCookie)
	}
}

func TestLoginOKAndSessionView(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login status %d", code)
	}
	if ts.session == "" {
		t.Fatal("login must set session cookie")
	}
	resp := ts.do(http.MethodGet, "/api/auth/session", "", nil)
	m := body(t, resp)
	if m["authenticated"] != true {
		t.Fatalf("want authenticated=true, got %v", m)
	}
	user, ok := m["user"].(map[string]any)
	if !ok || user["username"] != "alice" || user["is_admin"] != false {
		t.Fatalf("user payload: %v", m["user"])
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	resp := ts.do(http.MethodPost, "/api/auth/login",
		`{"username":"alice","password":"pw1"}`, map[string]string{
			HeaderCSRF: ts.csrfAfterBootstrap(t),
		})
	resp.Body.Close()
	var sess *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == CookieSession {
			sess = ck
		}
	}
	if sess == nil {
		t.Fatal("login must set session cookie")
	}
	if !sess.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if sess.Path != "/" || sess.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie attributes: %+v", sess)
	}
}

// csrfAfterBootstrap — bootstrap без сохранения csrf в клиент.
func (ts *testServer) csrfAfterBootstrap(t *testing.T) string {
	t.Helper()
	resp := ts.do(http.MethodGet, "/api/auth/session", "", nil)
	resp.Body.Close()
	return ts.csrf
}

func TestLoginBadCredentials(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	cases := []struct {
		name, username, password string
	}{
		{"wrong password", "alice", "nope"},
		{"unknown user", "bob", "pw1"},
	}
	for _, c := range cases {
		if code := ts.login(c.username, c.password); code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d", c.name, code)
		}
	}
}

func TestCSRFEnforcedOnStateChanging(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	// POST без заголовка X-CSRFToken — 403.
	req, _ := http.NewRequest(http.MethodPost, ts.server.URL+"/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: ts.session})
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post without csrf header: status %d, want 403", resp.StatusCode)
	}
	// Неверное значение заголовка — тоже 403.
	req2, _ := http.NewRequest(http.MethodPost, ts.server.URL+"/api/auth/logout", nil)
	req2.AddCookie(&http.Cookie{Name: CookieSession, Value: ts.session})
	req2.Header.Set(HeaderCSRF, "forged")
	resp2, err := ts.client.Do(req2)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("post with forged csrf: status %d, want 403", resp2.StatusCode)
	}
	// GET без заголовка проходит: чтение не требует CSRF.
	resp3 := ts.do(http.MethodGet, "/api/auth/session", "", nil)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("get without csrf: status %d", resp3.StatusCode)
	}
}

func TestSessionRotationOnLogin(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("first login: %d", code)
	}
	first := ts.session
	firstCSRF := ts.csrf
	// Второй вход с той же cookie: серверная сессия меняется.
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("second login: %d", code)
	}
	if ts.session == first {
		t.Fatal("session token must rotate on login")
	}
	if ts.csrf == firstCSRF {
		t.Fatal("csrf token must rotate on login")
	}
	// Старый токен мёртв: серверная сессия удалена.
	req, _ := http.NewRequest(http.MethodGet, ts.server.URL+"/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: first})
	oldResp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("do old token: %v", err)
	}
	m := body(t, oldResp)
	if m["authenticated"] != false {
		t.Fatal("old session token must be revoked after rotation")
	}
}

func TestLogoutKillsServerSession(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	token := ts.session
	resp := ts.do(http.MethodPost, "/api/auth/logout", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	// Cookie чистятся...
	if ts.session != "" || ts.csrf != "" {
		t.Fatalf("cookies must be cleared, got session=%q csrf=%q", ts.session, ts.csrf)
	}
	// ...и серверная сессия удалена: старый токен больше не работает.
	req, _ := http.NewRequest(http.MethodGet, ts.server.URL+"/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: token})
	oldResp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	m := body(t, oldResp)
	if m["authenticated"] != false {
		t.Fatal("logout must delete the server-side session")
	}
}

func TestDisabledUserRejectedMidSession(t *testing.T) {
	ts := newTestServer(t)
	u := ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	// Блокировка напрямую в базе: сервер обязан увидеть её на следующем
	// же запросе, права не заморожены в сессии.
	if err := ts.svc.store.SetUserDisabled(context.Background(), u.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	resp := ts.do(http.MethodGet, "/api/auth/session", "", nil)
	m := body(t, resp)
	if m["authenticated"] != false {
		t.Fatal("disabled user must be rejected mid-session")
	}
	// Вход заблокированного — тот же 401, без раскрытия причины.
	if code := ts.login("alice", "pw1"); code != http.StatusUnauthorized {
		t.Fatalf("disabled login: %d, want 401", code)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	token := ts.session
	// Имитируем время после истечения срока: Load обязан отказать.
	_, _, err := ts.svc.sessions.Load(context.Background(), token, time.Now().Add(31*24*time.Hour))
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired session: err=%v, want ErrSessionNotFound", err)
	}
}

func TestPasswordResetRevokesSessions(t *testing.T) {
	ts := newTestServer(t)
	admin := ts.addUser("root", "pw0", true)
	victim := ts.addUser("bob", "pw1", false)
	if code := ts.login("root", "pw0"); code != http.StatusNoContent {
		t.Fatalf("admin login: %d", code)
	}
	// Жертва входит отдельным клиентом.
	victimTS := &testServer{t: t, svc: ts.svc, server: ts.server, client: ts.client}
	if code := victimTS.login("bob", "pw1"); code != http.StatusNoContent {
		t.Fatalf("victim login: %d", code)
	}
	victimToken := victimTS.session
	resp := ts.do(http.MethodPost, "/api/admin/users/bob/password-reset",
		`{"new_password":"newpw"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("password-reset: %d", resp.StatusCode)
	}
	// Сессия жертвы отозвана.
	req, _ := http.NewRequest(http.MethodGet, ts.server.URL+"/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: victimToken})
	oldResp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if m := body(t, oldResp); m["authenticated"] != false {
		t.Fatal("password reset must revoke user sessions")
	}
	// Новый пароль работает.
	fresh := &testServer{t: t, svc: ts.svc, server: ts.server, client: ts.client}
	if code := fresh.login("bob", "newpw"); code != http.StatusNoContent {
		t.Fatalf("login with new password: %d", code)
	}
	_ = admin
	_ = victim
}

func TestPasswordResetRequiresAdmin(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("root", "pw0", true)
	ts.addUser("mallory", "pw1", false)
	if code := ts.login("mallory", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	resp := ts.do(http.MethodPost, "/api/admin/users/root/password-reset",
		`{"new_password":"x12345678"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin password-reset: %d, want 403", resp.StatusCode)
	}
	// Аноним — 401.
	resp2 := ts.doAnon(http.MethodPost, "/api/admin/users", `{"username":"x"}`)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous admin call: %d, want 401", resp2.StatusCode)
	}
}

// doAnon — запрос без cookie вообще (лимитер и CSRF анонима не должны
// мешать отказу по правам).
func (ts *testServer) doAnon(method, path, bodyStr string) *http.Response {
	ts.t.Helper()
	req, err := http.NewRequest(method, ts.server.URL+path, strings.NewReader(bodyStr))
	if err != nil {
		ts.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.client.Do(req)
	if err != nil {
		ts.t.Fatalf("do: %v", err)
	}
	return resp
}

func TestAdminCreateUser(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("root", "pw0", true)
	if code := ts.login("root", "pw0"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	resp := ts.do(http.MethodPost, "/api/admin/users",
		`{"username":"carol","password":"pwcarol","display_name":"Carol","is_admin":false}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d", resp.StatusCode)
	}
	m := body(t, resp)
	if m["username"] != "carol" {
		t.Fatalf("payload: %v", m)
	}
	// Дубликат — 409.
	resp2 := ts.do(http.MethodPost, "/api/admin/users",
		`{"username":"carol","password":"other"}`, nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate user: %d, want 409", resp2.StatusCode)
	}
	// Новый пользователь входит.
	fresh := &testServer{t: t, svc: ts.svc, server: ts.server, client: ts.client}
	if code := fresh.login("carol", "pwcarol"); code != http.StatusNoContent {
		t.Fatalf("created user login: %d", code)
	}
}

func TestAdminDisableEnableUser(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("root", "pw0", true)
	ts.addUser("bob", "pw1", false)
	if code := ts.login("root", "pw0"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	resp := ts.do(http.MethodPost, "/api/admin/users/bob/disable", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	u, err := ts.svc.store.GetUserByUsername(context.Background(), "bob")
	if err != nil || !u.Disabled {
		t.Fatalf("bob must be disabled: err=%v disabled=%v", err, u.Disabled)
	}
	resp2 := ts.do(http.MethodPost, "/api/admin/users/bob/enable", "", nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("enable: %d", resp2.StatusCode)
	}
	// Неизвестный пользователь — 404.
	resp3 := ts.do(http.MethodPost, "/api/admin/users/ghost/disable", "", nil)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("disable unknown: %d, want 404", resp3.StatusCode)
	}
}

func TestLastAdminGuardDisable(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("root", "pw0", true)
	ts.addUser("helper", "pw1", true)
	if code := ts.login("root", "pw0"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	// Один из двух админов — можно.
	resp := ts.do(http.MethodPost, "/api/admin/users/helper/disable", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable non-last admin: %d", resp.StatusCode)
	}
	// Последнего — нельзя: 409.
	resp2 := ts.do(http.MethodPost, "/api/admin/users/root/disable", "", nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("disable last admin: %d, want 409", resp2.StatusCode)
	}
	// root всё ещё активен и администратор.
	u, _ := ts.svc.store.GetUserByUsername(context.Background(), "root")
	if u.Disabled || !u.IsAdmin {
		t.Fatalf("last admin must stay active: %+v", u)
	}
}

func TestChangePasswordSelf(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	oldSession := ts.session
	resp := ts.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"pw1","new_password":"pw2"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: %d", resp.StatusCode)
	}
	// Текущему браузеру выдана новая сессия.
	if ts.session == "" || ts.session == oldSession {
		t.Fatal("change password must issue a fresh session")
	}
	// Старая сессия отозвана.
	req, _ := http.NewRequest(http.MethodGet, ts.server.URL+"/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: oldSession})
	oldResp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if m := body(t, oldResp); m["authenticated"] != false {
		t.Fatal("other sessions must be revoked on password change")
	}
	// Новый пароль работает, старый — нет.
	fresh := &testServer{t: t, svc: ts.svc, server: ts.server, client: ts.client}
	if code := fresh.login("alice", "pw2"); code != http.StatusNoContent {
		t.Fatalf("login with new password: %d", code)
	}
	fresh2 := &testServer{t: t, svc: ts.svc, server: ts.server, client: ts.client}
	if code := fresh2.login("alice", "pw1"); code != http.StatusUnauthorized {
		t.Fatalf("login with old password: %d, want 401", code)
	}
}

func TestChangePasswordWrongCurrent(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	resp := ts.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"wrong","new_password":"pw2"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong current password: %d, want 401", resp.StatusCode)
	}
	// Пароль не изменился.
	fresh := &testServer{t: t, svc: ts.svc, server: ts.server, client: ts.client}
	if code := fresh.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("original password must still work: %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	// 10 неудач — лимит; каждый заход заново бутстрапит csrf.
	for i := 0; i < 10; i++ {
		if code := ts.login("alice", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("fail %d: status %d, want 401", i, code)
		}
	}
	// Дальше — 429 даже с правильным паролем.
	if code := ts.login("alice", "pw1"); code != http.StatusTooManyRequests {
		t.Fatalf("limited login: %d, want 429", code)
	}
	// Другой username с того же IP: IP-счётчик тоже сработал.
	if code := ts.login("bob", "pw1"); code != http.StatusTooManyRequests {
		t.Fatalf("other username same ip: %d, want 429", code)
	}
	// Успешный вход сбрасывает счётчики своего ключа.
	ts2 := newTestServer(t)
	ts2.addUser("dave", "pw1", false)
	for i := 0; i < 9; i++ {
		if code := ts2.login("dave", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("fail %d: %d", i, code)
		}
	}
	if code := ts2.login("dave", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login before limit: %d", code)
	}
}

func TestAdminStatusNotBakedIntoSession(t *testing.T) {
	ts := newTestServer(t)
	u := ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	// alice ещё не админ: отказ.
	resp := ts.do(http.MethodPost, "/api/admin/users",
		`{"username":"x","password":"y12345678"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin create user: %d, want 403", resp.StatusCode)
	}
	// Права подняли в базе напрямую: тот же cookie теперь работает.
	if err := ts.svc.store.SetUserAdmin(context.Background(), u.ID, true); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	resp2 := ts.do(http.MethodPost, "/api/admin/users",
		`{"username":"x","password":"y12345678"}`, nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("admin after promotion: %d, want 201", resp2.StatusCode)
	}
}

func TestMiddlewareTagsLoggerWithUser(t *testing.T) {
	// Внешняя обёртка кладёт slog-логгер в контекст; middleware
	// аутентификации обязан дополнить его идентификатором пользователя.
	ts := newTestServer(t)
	ts.addUser("alice", "pw1", false)
	if code := ts.login("alice", "pw1"); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	records := make(chan *slogRecord, 4)
	logger := slog.New(&captureHandler{out: records})
	// Внутренний обработчик пишет одну запись логгером запроса;
	// проверяем, что middleware дополнил его идентификатором пользователя.
	inner := ts.svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		LoggerFromContext(r.Context()).Info("request handled")
	}))
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(WithLogger(r.Context(), logger)))
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: ts.session})
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	// Хендлер маршрута пишет одну запись — проверяем её атрибуты.
	select {
	case rec := <-records:
		if rec.attrs["auth_user_id"] == "" {
			t.Fatalf("record must carry auth_user_id: %v", rec.attrs)
		}
		if rec.attrs["auth_username"] != "alice" {
			t.Fatalf("record must carry auth_username: %v", rec.attrs)
		}
		for k, v := range rec.attrs {
			if strings.Contains(fmt.Sprint(v), "pw1") || strings.Contains(k, "password") {
				t.Fatalf("credentials must not be logged: %s=%v", k, v)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no log record captured")
	}
}

// slogRecord — перехваченная запись лога.
type slogRecord struct {
	attrs map[string]string
}

// captureHandler — slog-хендлер, складывающий записи; WithAttrs
// накапливается, чтобы увидеть дополнения от Logger.With.
type captureHandler struct {
	mu    sync.Mutex
	attrs []string // "key=value"
	out   chan *slogRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	h.mu.Lock()
	for _, kv := range h.attrs {
		if i := strings.IndexByte(kv, '='); i > 0 {
			attrs[kv[:i]] = kv[i+1:]
		}
	}
	h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.out <- &slogRecord{attrs: attrs}
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range attrs {
		h.attrs = append(h.attrs, a.Key+"="+a.Value.String())
	}
	return h
}
func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func TestErrorsAreJSONDetail(t *testing.T) {
	ts := newTestServer(t)
	// Проверяем формат отказа учётных данных после CSRF bootstrap.
	ts.bootstrap()
	resp := ts.do(http.MethodPost, "/api/auth/login",
		`{"username":"nobody","password":"x"}`, nil)
	m := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if d, ok := m["detail"].(string); !ok || d != "invalid credentials" {
		t.Fatalf("detail: %v", m)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("content type: %q", ct)
	}
}

func TestLoginRequiresClientBoundCSRFAndOrigin(t *testing.T) {
	s := NewService(testStore(t), Config{Secret: []byte("test secret"), Argon2: fastArgon2()}, nil)
	mux := http.NewServeMux()
	s.Mount(mux)
	h := s.Middleware(mux)
	bootstrap := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://brood.test/api/auth/session", nil))
		for _, c := range w.Result().Cookies() {
			if c.Name == CookieCSRF {
				return c.Value
			}
		}
		t.Fatal("no bootstrap CSRF cookie")
		return ""
	}
	a, b := bootstrap(), bootstrap()
	if a == b {
		t.Error("anonymous CSRF tokens shared between clients")
	}
	for _, tc := range []struct {
		name, cookie, header, origin string
		want                         int
	}{
		{"missing pair", "", "", "http://brood.test", 403},
		{"missing cookie", "", a, "http://brood.test", 403},
		{"missing header", a, "", "http://brood.test", 403},
		{"other client header", a, b, "http://brood.test", 403},
		{"unsigned pair", "forged", "forged", "http://brood.test", 403},
		{"missing origin", a, a, "", 403},
		{"cross origin", a, a, "http://evil.test", 403},
		{"null origin", a, a, "null", 403},
		{"wrong scheme", a, a, "https://brood.test", 403},
		{"CLI with origin and token", a, a, "http://brood.test", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://brood.test/api/auth/login", strings.NewReader(`{"username":"missing","password":"pw"}`))
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tc.cookie})
			}
			r.Header.Set(HeaderCSRF, tc.header)
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestCredentialRevocationWinsOverStaleAuthentication(t *testing.T) {
	for _, mutation := range []string{"disable", "password reset"} {
		t.Run(mutation, func(t *testing.T) {
			ts := newTestServer(t)
			u := ts.addUser("alice", "old", false)
			valid, checked := ts.svc.checkCredentials(t.Context(), "alice", "old")
			if !valid {
				t.Fatal("initial credentials rejected")
			}
			if err := ts.svc.store.WithTx(t.Context(), func(ctx context.Context) error {
				if mutation == "disable" {
					return ts.svc.store.SetUserDisabled(ctx, u.ID, true)
				}
				if err := ts.svc.store.SetPasswordHash(ctx, u.ID, "reset hash"); err != nil {
					return err
				}
				_, err := ts.svc.sessions.RevokeAllOfUser(ctx, u.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if token, _, err := ts.svc.rotate(t.Context(), checked, ""); err == nil || token != "" {
				t.Errorf("stale login issued session: token present=%v err=%v", token != "", err)
			}
			if token, _, err := ts.svc.changePassword(t.Context(), checked, "attacker hash"); err == nil || token != "" {
				t.Errorf("stale password change succeeded: token present=%v err=%v", token != "", err)
			}
			current, err := ts.svc.store.GetUser(t.Context(), u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "password reset" && current.PasswordHash != "reset hash" {
				t.Errorf("password reset overwritten by stale credentials")
			}
		})
	}
}

func TestClientIPIgnoresUntrustedForwarding(t *testing.T) {
	r := httptest.NewRequest("POST", "http://brood.test/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.2")
	r.Header.Set("Forwarded", "for=198.51.100.2")
	if got := ClientIP(r); got != "192.0.2.1" {
		t.Fatalf("spoofed peer: %s", got)
	}
}

func TestNoCredentialsInResponseBodies(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "secret-password-value", false)
	resp := ts.do(http.MethodPost, "/api/auth/login",
		`{"username":"alice","password":"wrong-guess"}`, map[string]string{
			HeaderCSRF: ts.csrfAfterBootstrap(t),
		})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "wrong-guess") ||
		strings.Contains(string(raw), "secret-password-value") {
		t.Fatalf("password leaked in response: %s", raw)
	}
}
