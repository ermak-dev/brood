// Сквозные проверки собранного сервера: httptest поверх Handler с
// временной базой и фейковым провижинером. Покрывают bootstrap-логику,
// вход, административный сценарий пользователь-проект-членство, рои,
// изоляцию чужих роёв и блокировку пользователя.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

// fakeProvisioner — фейк app.Provisioner: те же правила, что у
// Substrate-адаптера (чужой UID не удаляется, отсутствующая область —
// ErrNotFound).
type fakeProvisioner struct {
	mu        sync.Mutex
	atespaces map[string]string
	next      int
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{atespaces: map[string]string{}}
}

func (p *fakeProvisioner) CreateAtespace(name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.atespaces[name]; ok {
		return "", fmt.Errorf("%w: atespace %s exists", app.ErrConflict, name)
	}
	p.next++
	uid := fmt.Sprintf("uid-%d", p.next)
	p.atespaces[name] = uid
	return uid, nil
}

func (p *fakeProvisioner) GetAtespace(name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	uid, ok := p.atespaces[name]
	if !ok {
		return "", fmt.Errorf("%w: atespace %s", app.ErrNotFound, name)
	}
	return uid, nil
}

func (p *fakeProvisioner) DeleteAtespace(name, uid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.atespaces[name]
	if !ok {
		return fmt.Errorf("%w: atespace %s", app.ErrNotFound, name)
	}
	if uid != "" && current != uid {
		return fmt.Errorf("%w: atespace %s belongs to uid %s", app.ErrConflict, name, current)
	}
	delete(p.atespaces, name)
	return nil
}

// testServer — собранный сервер с временной базой.
type testServer struct {
	t            *testing.T
	srv          *Server
	http         *httptest.Server
	provisioner  app.Provisioner
	adminCreated bool
}

// newTestServer — сервер с фейковым провижинером (Substrate работает).
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServerWith(t, newFakeProvisioner())
}

// newTestServerWith — сервер с данным провижинером; nil — Substrate
// не настроен.
func newTestServerWith(t *testing.T, provisioner app.Provisioner) *testServer {
	t.Helper()
	cfg := Config{DBPath: t.TempDir() + "/brood.db"}
	if provisioner == nil {
		// nil-провижинер имитирует не настроенный Substrate: любое
		// обращение к atespace — 503 с именами настроек.
		provisioner = &stubProvisioner{}
	}
	srv, err := New(nil, cfg, Options{Provisioner: provisioner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := &testServer{t: t, srv: srv, provisioner: provisioner}
	ts.http = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.http.Close()
		srv.Close()
	})
	return ts
}

// stubProvisioner — провижинер не настроенного Substrate: отказ 503
// с именами настроек на каждой операции.
type stubProvisioner struct{}

func (p *stubProvisioner) CreateAtespace(string) (string, error) {
	return "", &notConfiguredError{detail: errAteapiNotConfiguredText}
}
func (p *stubProvisioner) GetAtespace(string) (string, error) {
	return "", &notConfiguredError{detail: errAteapiNotConfiguredText}
}
func (p *stubProvisioner) DeleteAtespace(string, string) error {
	return &notConfiguredError{detail: errAteapiNotConfiguredText}
}

// bootstrapAdmin заводит первого администратора тем же путём, что и
// команда бинарника: хэш Argon2id, идемпотентный BootstrapAdmin.
func (ts *testServer) bootstrapAdmin(username, password string) {
	ts.t.Helper()
	hash, err := auth.HashPassword(password, auth.DefaultArgon2())
	if err != nil {
		ts.t.Fatalf("hash password: %v", err)
	}
	ctx := nil2ctx()
	created, err := ts.srv.store.BootstrapAdmin(ctx, username, hash)
	if err != nil {
		ts.t.Fatalf("bootstrap admin: %v", err)
	}
	if created {
		ts.adminCreated = true
	}
}

// newClient — клиент с cookie-jar (как браузер).
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{Jar: jar}
}

// nil2ctx — контекст по умолчанию.
func nil2ctx() context.Context { return context.Background() }

// do выполняет запрос к тестовому серверу; client держит cookie-jar.
func (ts *testServer) do(client *http.Client, method, path, token string, body any) (*http.Response, map[string]any, string) {
	ts.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			ts.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.http.URL+path, reader)
	if err != nil {
		ts.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", ts.http.URL)
	}
	if token != "" {
		req.Header.Set(auth.HeaderCSRF, token)
	}
	resp, err := client.Do(req)
	if err != nil {
		ts.t.Fatalf("%s %s: %v", method, path, err)
	}
	rawBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		ts.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	raw := string(rawBytes)
	var data map[string]any
	_ = json.Unmarshal(rawBytes, &data)
	return resp, data, raw
}

// csrf достаёт текущее значение csrf cookie клиента.
func (ts *testServer) csrf(client *http.Client) string {
	ts.t.Helper()
	req, err := http.NewRequest("GET", ts.http.URL+"/api/auth/session", nil)
	if err != nil {
		ts.t.Fatalf("new request: %v", err)
	}
	u := req.URL
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == auth.CookieCSRF {
			return c.Value
		}
	}
	return ""
}

// login входит как username; после входа csrf ротируется.
func (ts *testServer) login(client *http.Client, username, password string) {
	ts.t.Helper()
	resp, _, _ := ts.do(client, "GET", "/api/auth/session", "", nil)
	if resp.StatusCode != http.StatusOK {
		ts.t.Fatalf("session bootstrap: status %d", resp.StatusCode)
	}
	resp, data, _ := ts.do(client, "POST", "/api/auth/login", ts.csrf(client),
		map[string]string{"username": username, "password": password})
	if resp.StatusCode != http.StatusNoContent {
		ts.t.Fatalf("login %s: status %d, body %v", username, resp.StatusCode, data)
	}
}

// expectStatus сверяет статус и причину отказа.
func expectStatus(t *testing.T, what string, resp *http.Response, data map[string]any, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s: status = %d, want %d, body %v", what, resp.StatusCode, want, data)
	}
}

// expectDetail сверяет причину отказа.
func expectDetail(t *testing.T, what string, data map[string]any, want string) {
	t.Helper()
	if got, _ := data["detail"].(string); got != want {
		t.Fatalf("%s: detail = %q, want %q", what, got, want)
	}
}

// TestHealthzAndStatic — проба готовности и встроенная панель анонимно.
func TestHealthzAndStatic(t *testing.T) {
	ts := newTestServer(t)
	client := newClient(t)

	resp, _, body := ts.do(client, "GET", "/healthz", "", nil)
	expectStatus(t, "healthz", resp, nil, http.StatusOK)
	if !strings.Contains(body, "ok") {
		t.Fatalf("healthz body = %q", body)
	}

	resp, _, body = ts.do(client, "GET", "/", "", nil)
	expectStatus(t, "index", resp, nil, http.StatusOK)
	if !strings.Contains(body, "/api/auth/login") {
		t.Fatalf("index has no login flow")
	}

	resp, _, _ = ts.do(client, "GET", "/nope", "", nil)
	expectStatus(t, "unknown path", resp, nil, http.StatusNotFound)
}

// TestLoginFlow — вход, данные сессии, отказ и выход.
func TestLoginFlow(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "root-password-1")
	client := newClient(t)

	// Анонимная сессия: csrf cookie и authenticated=false.
	resp, data, _ := ts.do(client, "GET", "/api/auth/session", "", nil)
	expectStatus(t, "anonymous session", resp, data, http.StatusOK)
	if data["authenticated"] != false {
		t.Fatalf("anonymous session: %v", data)
	}
	if ts.csrf(client) == "" {
		t.Fatal("anonymous session did not set csrf cookie")
	}

	// Неверный пароль: 401 без раскрытия.
	resp, data, _ = ts.do(client, "POST", "/api/auth/login", ts.csrf(client),
		map[string]string{"username": "root", "password": "wrong"})
	expectStatus(t, "wrong password", resp, data, http.StatusUnauthorized)
	expectDetail(t, "wrong password", data, "invalid credentials")

	// Несуществующий пользователь: тот же ответ.
	resp, data, _ = ts.do(client, "POST", "/api/auth/login", ts.csrf(client),
		map[string]string{"username": "ghost", "password": "whatever"})
	expectStatus(t, "unknown user", resp, data, http.StatusUnauthorized)
	expectDetail(t, "unknown user", data, "invalid credentials")

	// Успешный вход: 204, сессия показывает администратора.
	ts.login(client, "root", "root-password-1")
	resp, data, _ = ts.do(client, "GET", "/api/auth/session", "", nil)
	expectStatus(t, "session", resp, data, http.StatusOK)
	if data["authenticated"] != true {
		t.Fatalf("session: %v", data)
	}
	user, _ := data["user"].(map[string]any)
	if user["username"] != "root" || user["is_admin"] != true {
		t.Fatalf("session user: %v", user)
	}

	// Logout отзывает сессию.
	resp, data, _ = ts.do(client, "POST", "/api/auth/logout", ts.csrf(client), nil)
	expectStatus(t, "logout", resp, data, http.StatusNoContent)
	resp, data, _ = ts.do(client, "GET", "/api/auth/session", "", nil)
	if data["authenticated"] != false {
		t.Fatalf("session after logout: %v", data)
	}
}

// TestAdminScenario — администратор заводит пользователей, проект
// и членство; участники создают рои; изоляция чужих роёв; блокировка.
func TestAdminScenario(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "root-password-1")

	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")

	// Пользователи.
	for _, name := range []string{"alice", "bob"} {
		resp, data, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin),
			map[string]any{"username": name, "password": name + "-password-1"})
		expectStatus(t, "create user "+name, resp, data, http.StatusCreated)
	}
	// Дубликат имени — 409.
	resp, data, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin),
		map[string]any{"username": "alice", "password": "other-password-1"})
	expectStatus(t, "duplicate user", resp, data, http.StatusConflict)

	// Проект.
	resp, data, _ = ts.do(admin, "POST", "/api/projects", ts.csrf(admin),
		map[string]string{"name": "proj"})
	expectStatus(t, "create project", resp, data, http.StatusCreated)
	projectID, _ := data["id"].(string)
	if projectID == "" {
		t.Fatalf("create project: no id, body %v", data)
	}
	// Пустое имя — 400.
	resp, data, _ = ts.do(admin, "POST", "/api/projects", ts.csrf(admin),
		map[string]string{"name": ""})
	expectStatus(t, "empty project name", resp, data, http.StatusBadRequest)

	// Членство: alice и bob.
	for _, name := range []string{"alice", "bob"} {
		resp, data, _ = ts.do(admin, "POST", "/api/projects/"+projectID+"/members",
			ts.csrf(admin), map[string]string{"username": name})
		expectStatus(t, "add member "+name, resp, data, http.StatusNoContent)
	}
	// Повторное добавление — 409.
	resp, data, _ = ts.do(admin, "POST", "/api/projects/"+projectID+"/members",
		ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "duplicate member", resp, data, http.StatusConflict)
	// Несуществующий пользователь — 404.
	resp, data, _ = ts.do(admin, "POST", "/api/projects/"+projectID+"/members",
		ts.csrf(admin), map[string]string{"username": "ghost"})
	expectStatus(t, "member ghost", resp, data, http.StatusNotFound)
	// Несуществующий проект — 404.
	resp, data, _ = ts.do(admin, "POST", "/api/projects/missing/members",
		ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "member of missing project", resp, data, http.StatusNotFound)

	// Список пользователей — только админ, без хэшей.
	resp, data, _ = ts.do(admin, "GET", "/api/admin/users", "", nil)
	expectStatus(t, "list users", resp, data, http.StatusOK)
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "argon2") {
		t.Fatal("user list leaks password hashes")
	}

	// Не-админ не управляет пользователями.
	alice := newClient(t)
	ts.login(alice, "alice", "alice-password-1")
	resp, data, _ = ts.do(alice, "POST", "/api/admin/users", ts.csrf(alice),
		map[string]any{"username": "eve", "password": "eve-password-1"})
	expectStatus(t, "non-admin create user", resp, data, http.StatusForbidden)

	// Не-админ не создаёт проекты.
	resp, data, _ = ts.do(alice, "POST", "/api/projects", ts.csrf(alice),
		map[string]string{"name": "side"})
	expectStatus(t, "non-admin create project", resp, data, http.StatusForbidden)

	// Участник видит проект в своём списке (только свой; пустой список
	// декодируется в nil, поэтому сверяем сырое тело).
	resp, _, rawProjects := ts.do(alice, "GET", "/api/projects", "", nil)
	expectStatus(t, "alice projects", resp, nil, http.StatusOK)
	if !strings.Contains(rawProjects, `"name":"proj"`) || strings.Count(rawProjects, "name") != 1 {
		t.Fatalf("alice projects: %s", rawProjects)
	}

	// Аноним не видит ничего.
	anon := newClient(t)
	resp, data, _ = ts.do(anon, "GET", "/api/projects", "", nil)
	expectStatus(t, "anonymous projects", resp, data, http.StatusUnauthorized)

	// Рои: alice и bob создают по своему.
	resp, data, _ = ts.do(alice, "POST", "/api/projects/"+projectID+"/swarms",
		ts.csrf(alice), map[string]string{"name": "alpha"})
	expectStatus(t, "alice swarm", resp, data, http.StatusCreated)
	aliceSwarm, _ := data["id"].(string)
	if aliceSwarm == "" || data["state"] != "active" || data["owner_id"] == "" {
		t.Fatalf("alice swarm: %v", data)
	}
	if name, _ := data["atespace_name"].(string); !strings.HasPrefix(name, "swarm-") {
		t.Fatalf("atespace name: %v", data["atespace_name"])
	}

	bob := newClient(t)
	ts.login(bob, "bob", "bob-password-1")
	resp, data, _ = ts.do(bob, "POST", "/api/projects/"+projectID+"/swarms",
		ts.csrf(bob), map[string]string{"name": "beta"})
	expectStatus(t, "bob swarm", resp, data, http.StatusCreated)
	bobSwarm, _ := data["id"].(string)

	// Чужой рой скрыт: 404, не 403 — существование не раскрывается.
	resp, data, _ = ts.do(bob, "GET", "/api/swarms/"+aliceSwarm, "", nil)
	expectStatus(t, "bob reads alice swarm", resp, data, http.StatusNotFound)
	// И не удаляется.
	resp, data, _ = ts.do(bob, "DELETE", "/api/swarms/"+aliceSwarm, ts.csrf(bob), nil)
	expectStatus(t, "bob deletes alice swarm", resp, data, http.StatusForbidden)

	// Каждый видит только свои рои в проекте.
	resp, _, rawSwarms := ts.do(bob, "GET", "/api/projects/"+projectID+"/swarms", "", nil)
	expectStatus(t, "bob swarms", resp, nil, http.StatusOK)
	if !strings.Contains(rawSwarms, `"id":"`+bobSwarm+`"`) ||
		strings.Contains(rawSwarms, `"id":"`+aliceSwarm+`"`) {
		t.Fatalf("bob swarms: %s", rawSwarms)
	}

	// Администратор видит и передаёт владение.
	resp, data, _ = ts.do(admin, "GET", "/api/swarms/"+aliceSwarm, "", nil)
	expectStatus(t, "admin reads alice swarm", resp, data, http.StatusOK)
	resp, data, _ = ts.do(admin, "POST", "/api/swarms/"+bobSwarm+"/transfer",
		ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "transfer to non-member-check", resp, data, http.StatusNoContent)
	resp, data, _ = ts.do(alice, "GET", "/api/swarms/"+bobSwarm, "", nil)
	expectStatus(t, "alice reads transferred swarm", resp, data, http.StatusOK)
	// Передача не-участнику — 403.
	resp, data, _ = ts.do(admin, "POST", "/api/swarms/"+aliceSwarm+"/transfer",
		ts.csrf(admin), map[string]string{"username": "root"})
	expectStatus(t, "transfer to admin non-member", resp, data, http.StatusForbidden)

	// Удаление роя владельцем; повторное чтение — 404.
	resp, data, _ = ts.do(alice, "DELETE", "/api/swarms/"+aliceSwarm, ts.csrf(alice), nil)
	expectStatus(t, "alice deletes swarm", resp, data, http.StatusNoContent)
	resp, data, _ = ts.do(alice, "GET", "/api/swarms/"+aliceSwarm, "", nil)
	expectStatus(t, "deleted swarm", resp, data, http.StatusNotFound)

	// Исключение из проекта отзывает доступ, рои остаются.
	resp, data, _ = ts.do(admin, "DELETE", "/api/projects/"+projectID+"/members/alice",
		ts.csrf(admin), nil)
	expectStatus(t, "remove member", resp, data, http.StatusNoContent)
	resp, data, _ = ts.do(alice, "GET", "/api/projects/"+projectID+"/swarms", "", nil)
	expectStatus(t, "removed member swarms", resp, data, http.StatusForbidden)

	// Блокировка пользователя отзывает сессию: запросы анонимны.
	resp, data, _ = ts.do(admin, "POST", "/api/admin/users/bob/disable",
		ts.csrf(admin), nil)
	expectStatus(t, "disable bob", resp, data, http.StatusNoContent)
	resp, data, _ = ts.do(bob, "GET", "/api/projects", "", nil)
	expectStatus(t, "disabled bob", resp, data, http.StatusUnauthorized)
	resp, data, _ = ts.do(bob, "GET", "/api/auth/session", "", nil)
	if data["authenticated"] != false {
		t.Fatalf("disabled bob session: %v", data)
	}

	// Удаление проекта — только админ.
	resp, data, _ = ts.do(admin, "DELETE", "/api/projects/"+projectID, ts.csrf(admin), nil)
	expectStatus(t, "delete project", resp, data, http.StatusNoContent)
	resp, data, _ = ts.do(admin, "DELETE", "/api/projects/"+projectID, ts.csrf(admin), nil)
	expectStatus(t, "delete missing project", resp, data, http.StatusNotFound)
}

// TestSwarmUnconfiguredSubstrate — без настроек Substrate создание роя
// фиксирует error/retry, повтор после настройки доходит до active.
func TestSwarmUnconfiguredSubstrate(t *testing.T) {
	ts := newTestServerWith(t, nil)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")

	resp, data, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin),
		map[string]any{"username": "alice", "password": "alice-password-1"})
	expectStatus(t, "create user", resp, data, http.StatusCreated)
	resp, data, _ = ts.do(admin, "POST", "/api/projects", ts.csrf(admin),
		map[string]string{"name": "proj"})
	expectStatus(t, "create project", resp, data, http.StatusCreated)
	projectID, _ := data["id"].(string)
	resp, data, _ = ts.do(admin, "POST", "/api/projects/"+projectID+"/members",
		ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "add member", resp, data, http.StatusNoContent)

	alice := newClient(t)
	ts.login(alice, "alice", "alice-password-1")
	resp, data, _ = ts.do(alice, "POST", "/api/projects/"+projectID+"/swarms",
		ts.csrf(alice), map[string]string{"name": "alpha"})
	expectStatus(t, "swarm create", resp, data, http.StatusServiceUnavailable)
	rows, err := ts.srv.store.ListSwarmsByProject(context.Background(), projectID)
	if err != nil || len(rows) != 1 || rows[0].State != "error" || rows[0].Retry != "create" {
		t.Fatalf("lost durable failed creation: %+v, %v", rows, err)
	}
}

// TestSubstrateStubs — сквозные маршруты actor API Python-сервера
// заменены роевой формой (#32); покрытие живёт в server_actors_test.go,
// здесь только фиксация отсутствия заглушек 501.
func TestSubstrateStubs(t *testing.T) {
	ts := newTestServer(t)
	client := newClient(t)
	for _, path := range []string{"/api/projects/p1/actors", "/api/workers", "/api/policy/global"} {
		resp, _, _ := ts.do(client, "GET", path, "", nil)
		if resp.StatusCode == http.StatusNotImplemented {
			t.Fatalf("stub still mounted at %s", path)
		}
	}
}

// TestWorkerPoolsUnconfigured — без настроек Kubernetes эндпоинты пула
// отказывают 503 с именами настроек; не-админу — 403 раньше настроек.
func TestWorkerPoolsUnconfigured(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")

	resp, data, _ := ts.do(admin, "GET", "/api/worker-pools", "", nil)
	expectStatus(t, "unconfigured pools", resp, data, http.StatusServiceUnavailable)
	raw, _ := json.Marshal(data)
	if !strings.Contains(string(raw), "KUBE_API_URL") {
		t.Fatalf("pools refusal does not name settings: %v", data)
	}

	anon := newClient(t)
	resp, data, _ = ts.do(anon, "GET", "/api/worker-pools", "", nil)
	expectStatus(t, "anonymous pools", resp, data, http.StatusUnauthorized)
}

// TestProjectPersistence — данные переживают пересборку сервера на той
// же базе (каталог читает ту же схему).
func TestProjectPersistence(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DBPath: dir + "/brood.db"}

	srv1, err := New(nil, cfg, Options{Provisioner: newFakeProvisioner()})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	hash, err := auth.HashPassword("root-password-1", auth.DefaultArgon2())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := srv1.store.BootstrapAdmin(nil2ctx(), "root", hash); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	ctx := nil2ctx()
	project := store.Project{ID: store.NewID(), Name: "proj"}
	if err := srv1.store.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := srv1.store.AddMember(ctx, project.ID, mustUserID(t, srv1)); err != nil {
		t.Fatalf("add member: %v", err)
	}
	srv1.Close()

	srv2, err := New(nil, cfg, Options{Provisioner: newFakeProvisioner()})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer srv2.Close()

	gateway := StoreGateway(srv2.store, srv2.catalog)
	projects, err := gateway.ProjectsForUser(mustUserID(t, srv2))
	if err != nil {
		t.Fatalf("projects for user: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "proj" {
		t.Fatalf("projects for user: %v", projects)
	}
	swarms, err := gateway.Swarms()
	if err != nil {
		t.Fatalf("swarms: %v", err)
	}
	if len(swarms) != 0 {
		t.Fatalf("swarms: %v", swarms)
	}
}

// mustUserID возвращает ID первого пользователя базы (администратора
// проверки).
func mustUserID(t *testing.T, srv *Server) string {
	t.Helper()
	users, err := srv.store.ListUsers(nil2ctx())
	if err != nil || len(users) == 0 {
		t.Fatalf("list users: %v (%d)", err, len(users))
	}
	return users[0].ID
}
