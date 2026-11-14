package server

// BUG: #303: серверные fixtures многократно считают production Argon2 и тратят бюджет CI.
// HYPOTHESIS: дешёвые параметры настоящего Argon2 в harness уберут повторную стоимость,
// не меняя проверки паролей, сессий, CSRF и производственные умолчания.
// SOLUTION: закрытая зависимость Options передаёт параметры в auth.Config;
// общие fixtures используют 64 KiB, один проход и один поток.
// RESULT: FIXED — реальные парольные проверки сохранены, production defaults
// проверяются отдельно; локальный пакет: 242.683s → 80.782s, не замер раннера.
// STATUS: FIXED

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
)

func TestServerHarnessPasswordCost(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(*testing.T) *testServer
	}{
		{"plain", newTestServer},
		{"substrate", func(t *testing.T) *testServer {
			return newSubstrateServer(t, newFakeAteapi()).testServer
		}},
		{"telegram", func(t *testing.T) *testServer {
			return newTelegramHarness(t, "", nil).testServer
		}},
		{"management", func(t *testing.T) *testServer {
			return newLLMTestServer(t, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := tc.new(t)
			ts.bootstrapAdmin("root", "root-password-1")
			assertStoredPassword(t, ts.srv, "root", "root-password-1", "$argon2id$v=19$m=64,t=1,p=1$")
			admin := newClient(t)
			ts.login(admin, "root", "root-password-1")
			resp, data, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin),
				map[string]string{"username": "alice", "password": "alice-password-1"})
			expectStatus(t, "create fixture user", resp, data, http.StatusCreated)
			assertStoredPassword(t, ts.srv, "alice", "alice-password-1", "$argon2id$v=19$m=64,t=1,p=1$")
			alice := newClient(t)
			ts.login(alice, "alice", "alice-password-1")
			resp, data, _ = ts.do(alice, "POST", "/api/auth/login", ts.csrf(alice),
				map[string]string{"username": "alice", "password": "incorrect-password"})
			expectStatus(t, "wrong fixture password", resp, data, http.StatusUnauthorized)
		})
	}
}

// Нулевые Options обязаны сохранить production cost, даже если тестовый
// bootstrapAdmin создаёт свой дешёвый хэш.
func TestServerDefaultPasswordCost(t *testing.T) {
	srv, err := New(t.Context(), Config{DBPath: t.TempDir() + "/brood.db"}, Options{Provisioner: newFakeProvisioner()})
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{t: t, srv: srv, http: httptest.NewServer(srv.Handler())}
	t.Cleanup(func() { ts.http.Close(); srv.Close() })
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	resp, data, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin),
		map[string]string{"username": "alice", "password": "alice-password-1"})
	expectStatus(t, "create production user", resp, data, http.StatusCreated)
	assertStoredPassword(t, srv, "alice", "alice-password-1", "$argon2id$v=19$m=65536,t=2,p=4$")
}

func assertStoredPassword(t *testing.T, srv *Server, username, password, prefix string) {
	t.Helper()
	user, err := srv.store.GetUserByUsername(t.Context(), username)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(user.PasswordHash, prefix) {
		t.Errorf("%s password cost does not match %s", username, prefix)
	}
	if ok, err := auth.VerifyPassword(password, user.PasswordHash); !ok || err != nil {
		t.Fatalf("%s password does not verify: %v", username, err)
	}
	if ok, err := auth.VerifyPassword("incorrect-password", user.PasswordHash); ok || err != nil {
		t.Fatalf("%s wrong password was not rejected: %v", username, err)
	}
}
