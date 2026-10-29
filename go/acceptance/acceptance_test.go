// FEATURE: Installed binaries preserve authentication, isolation, and durable state.
// SPEC: No HTTP/auth substitutes; unavailable Substrate returns 503, never a
// successful actor claim. Revoked sessions exit 2; membership revokes resources.
package acceptance

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	alicePassword = "alice-acceptance-password-9813"
	bobPassword   = "bob-acceptance-password-5632"
)

func TestRealBinaries(t *testing.T) {
	bin := buildBinaries(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, binaries)
	}{
		{"CLIAndRestartPersistence", testCLIAndRestart},
		{"BrowserSecurityAndAuthorization", testBrowserSecurity},
		{"NoSubstrateDurableOwnerOnlyError", testNoSubstrate},
		{"DisablePasswordResetAndMembershipRevocation", testRevocation},
		{"SessionExpiry", testSessionExpiry},
		{"NegativeCLIArguments", testNegativeCLI},
		{"ActiveSwarmWithTLSInfrastructureFake", testInfrastructureFake},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, bin) })
	}
}

type project struct{ ID, Name string }
type swarm struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	OwnerID      string `json:"owner_id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	AtespaceName string `json:"atespace_name"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type setup struct {
	f                 *installation
	admin, alice, bob *browser
	shared, hidden    project
}

func setUp(t *testing.T, bin binaries, extra ...string) setup {
	t.Helper()
	f := newInstallation(t, bin, extra...)
	admin := f.browser()
	admin.login("admin", f.passwords[0])
	f.passwords = append(f.passwords, alicePassword, bobPassword)
	for _, user := range []struct{ name, password string }{{"alice", alicePassword}, {"bob", bobPassword}} {
		admin.expect("POST", "/api/admin/users", map[string]any{"username": user.name, "password": user.password, "display_name": user.name}, 201)
	}
	var shared, hidden project
	decode(t, admin.expect("POST", "/api/projects", map[string]string{"name": "shared-project"}, 201), &shared)
	decode(t, admin.expect("POST", "/api/projects", map[string]string{"name": "admin-private-project"}, 201), &hidden)
	if shared.ID == "" || hidden.ID == "" || shared.ID == hidden.ID {
		t.Fatal("created projects must have distinct nonempty IDs")
	}
	for _, username := range []string{"alice", "bob"} {
		admin.expect("POST", "/api/projects/"+shared.ID+"/members", map[string]string{"username": username}, 204)
	}
	alice, bob := f.browser(), f.browser()
	alice.login("alice", alicePassword)
	bob.login("bob", bobPassword)
	return setup{f, admin, alice, bob, shared, hidden}
}

func expectProjects(t *testing.T, b *browser, want ...project) {
	t.Helper()
	var got []project
	decode(t, b.expect("GET", "/api/projects", nil, 200), &got)
	if len(got) != len(want) {
		t.Fatalf("project count %d, want %d", len(got), len(want))
	}
	for _, expected := range want {
		found := false
		for _, p := range got {
			found = found || p == expected
		}
		if !found {
			t.Errorf("expected project %s not visible", expected.Name)
		}
	}
}

func expectCLIContains(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("CLI output does not contain %q", want)
	}
}

func testCLIAndRestart(t *testing.T, bin binaries) {
	s := setUp(t, bin)
	f := s.f
	f.cli("alice-state", "alice\n"+alicePassword+"\n", 0, "login", f.origin)
	expectCLIContains(t, f.cli("alice-state", "", 0, "whoami"), "alice")
	out := f.cli("alice-state", "", 0, "projects", "list")
	expectCLIContains(t, out, s.shared.ID+" "+s.shared.Name)
	if strings.Contains(out, s.hidden.ID) || strings.Contains(out, s.hidden.Name) {
		t.Error("CLI revealed a nonmember project")
	}
	f.auditFiles()
	f.stop()
	f.start()
	// Те же cookie и файлы bro, без повторного login после перезапуска.
	expectCLIContains(t, f.cli("alice-state", "", 0, "whoami"), "alice")
	expectProjects(t, s.alice, s.shared)
	expectProjects(t, s.admin, s.shared, s.hidden)
	var users []struct{ Username string }
	decode(t, s.admin.expect("GET", "/api/admin/users", nil, 200), &users)
	if len(users) != 3 {
		t.Errorf("users after restart: %d, want 3", len(users))
	}
	for _, want := range []string{"admin", "alice", "bob"} {
		found := false
		for _, user := range users {
			found = found || user.Username == want
		}
		if !found {
			t.Errorf("user %s missing after restart", want)
		}
	}
	// Проверяем серверный отзыв, а не только удаление файла bro.
	stolen := f.browser()
	stolen.client.Jar.SetCookies(stolen.base, cliCookies(t, f, "alice-state"))
	f.cli("alice-state", "", 0, "logout")
	stolen.expect("GET", "/api/projects", nil, 401)
	for range 2 {
		f.cli("alice-state", "", 2, "whoami", f.origin)
	}
	f.cli("alice-state", "alice\nwrong-acceptance-password\n", 2, "login", f.origin)
}

func cliCookies(t *testing.T, f *installation, state string) []*http.Cookie {
	t.Helper()
	// Читаем фактически записанный CLI state, не подменяя реализацию клиента.
	data := readCLIState(t, f, state)
	var saved struct{ Session, CSRF string }
	decode(t, data, &saved)
	if saved.Session == "" || saved.CSRF == "" {
		t.Fatal("CLI did not save a session and CSRF")
	}
	return []*http.Cookie{{Name: "session", Value: saved.Session, Path: "/"}, {Name: "csrf", Value: saved.CSRF, Path: "/"}}
}

func testBrowserSecurity(t *testing.T, bin binaries) {
	s := setUp(t, bin)
	anonymous := s.f.browser()
	code, _, headers := anonymous.request("GET", "/api/auth/session", nil, nil)
	if code != 200 || headers.Get("Cache-Control") != "no-store" {
		t.Fatal("anonymous session bootstrap must be uncached and successful")
	}
	for _, raw := range headers.Values("Set-Cookie") {
		resp := &http.Response{Header: http.Header{"Set-Cookie": []string{raw}}}
		for _, cookie := range resp.Cookies() {
			if cookie.Name == "csrf" && (cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/") {
				t.Error("CSRF cookie attributes violate the browser contract")
			}
		}
	}
	credentials := map[string]string{"username": "alice", "password": alicePassword}
	for _, h := range []map[string]string{
		{"Origin": "http://untrusted.invalid"},
		{"Origin": ""},
		{"X-CSRFToken": ""},
		{"X-CSRFToken": "incorrect"},
	} {
		code, _, _ := anonymous.request("POST", "/api/auth/login", credentials, h)
		if code != 403 {
			t.Errorf("login with invalid Origin/CSRF: status %d, want 403", code)
		}
	}
	for _, username := range []string{"alice", "nonexistent"} {
		body := anonymous.expect("POST", "/api/auth/login", map[string]string{"username": username, "password": "wrong-password"}, 401)
		var detail struct{ Detail string }
		decode(t, body, &detail)
		if detail.Detail != "invalid credentials" {
			t.Error("failed login disclosed credential validity")
		}
	}
	// Проверяем Set-Cookie сервера, а не атрибуты, отброшенные cookiejar.
	_, _, headers = anonymous.request("POST", "/api/auth/login", credentials, nil)
	resp := &http.Response{Header: headers}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == "session" {
			found = true
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
				t.Error("session cookie attributes violate the browser contract")
			}
		}
	}
	if !found {
		t.Fatal("successful login omitted session cookie")
	}
	code, _, _ = s.alice.request("POST", "/api/projects", map[string]string{"name": "csrf-bypass"}, map[string]string{"X-CSRFToken": ""})
	if code != 403 {
		t.Errorf("authenticated mutation without CSRF: %d, want 403", code)
	}
	expectProjects(t, s.alice, s.shared)
	expectProjects(t, s.bob, s.shared)
	s.alice.expect("POST", "/api/projects", map[string]string{"name": "privilege-bypass"}, 403)
	s.alice.expect("GET", "/api/admin/users", nil, 403)
	s.alice.expect("POST", "/api/admin/users", map[string]string{"username": "intruder", "password": "not-created"}, 403)
	s.alice.expect("POST", "/api/projects/"+s.hidden.ID+"/members", map[string]string{"username": "alice"}, 403)
	s.alice.expect("GET", "/api/projects/"+s.hidden.ID+"/swarms", nil, 403)
	s.alice.expect("POST", "/api/projects/"+s.hidden.ID+"/swarms", map[string]string{"name": "nonmember-bypass"}, 403)
	s.alice.expect("DELETE", "/api/projects/"+s.shared.ID, nil, 403)
	s.admin.expect("POST", "/api/projects", map[string]string{"name": ""}, 400)
	expectProjects(t, s.admin, s.shared, s.hidden)
	anonymous.expect("POST", "/api/auth/logout", nil, 204)
	if session, csrf := anonymous.cookies(); session != "" || csrf != "" {
		t.Error("logout did not clear browser cookies")
	}
	anonymous.expect("GET", "/api/projects", nil, 401)
}

func createFailedSwarm(t *testing.T, b *browser, p project, name string) swarm {
	t.Helper()
	path := "/api/projects/" + p.ID + "/swarms"
	code, body, _ := b.request("POST", path, map[string]string{"name": name}, nil)
	if code != 503 {
		t.Errorf("create without Substrate status %d, want 503 (durable error is not successful provisioning)", code)
	} else {
		var detail struct{ Detail string }
		decode(t, body, &detail)
		if detail.Detail == "" || !strings.Contains(detail.Detail, "ATEAPI_") {
			t.Error("unavailable Substrate must explain the missing configuration")
		}
	}
	var rows []swarm
	decode(t, b.expect("GET", path, nil, 200), &rows)
	for _, row := range rows {
		if row.Name == name {
			assertSwarm(t, row, p.ID, "error")
			return row
		}
	}
	t.Fatal("failed provisioning did not leave a durable visible error row")
	return swarm{}
}

func assertSwarm(t *testing.T, row swarm, projectID, state string) {
	t.Helper()
	if row.ID == "" || row.OwnerID == "" || row.ProjectID != projectID || row.State != state || row.AtespaceName != "swarm-"+strings.ToLower(strings.ReplaceAll(row.ID, "-", "")) {
		t.Errorf("invalid durable swarm identity/state: %+v", row)
	}
	for _, value := range []string{row.CreatedAt, row.UpdatedAt} {
		stamp, err := time.Parse(time.RFC3339, value)
		if err != nil || stamp.IsZero() || stamp.After(time.Now().Add(time.Second)) || !strings.HasSuffix(value, "Z") {
			t.Errorf("durable swarm timestamp must be nonzero UTC RFC3339 and not in the future: %q", value)
		}
	}
}

func testNoSubstrate(t *testing.T, bin binaries) {
	s := setUp(t, bin)
	alice := createFailedSwarm(t, s.alice, s.shared, "alice-failed")
	bob := createFailedSwarm(t, s.bob, s.shared, "bob-failed")
	if alice.OwnerID == bob.OwnerID {
		t.Error("different creators received the same swarm owner")
	}
	check := func() {
		t.Helper()
		for _, user := range []struct {
			browser *browser
			own     swarm
			other   swarm
		}{{s.alice, alice, bob}, {s.bob, bob, alice}} {
			var rows []swarm
			decode(t, user.browser.expect("GET", "/api/projects/"+s.shared.ID+"/swarms", nil, 200), &rows)
			if len(rows) != 1 || rows[0].ID != user.own.ID {
				t.Error("project membership exposed another owner's swarm or lost the durable error row")
			}
			var own swarm
			decode(t, user.browser.expect("GET", "/api/swarms/"+user.own.ID, nil, 200), &own)
			assertSwarm(t, own, s.shared.ID, "error")
			user.browser.expect("GET", "/api/swarms/"+user.other.ID, nil, 404)
			code, _, _ := user.browser.request("GET", "/api/swarms/"+user.other.ID+"/actors", nil, nil)
			if code != 404 {
				t.Errorf("non-owner actor list status %d, want 404 even without Substrate", code)
			}
			user.browser.expect("POST", "/api/swarms/"+user.other.ID+"/transfer", map[string]string{"username": "bob"}, 404)
		}
	}
	check()
	s.f.stop()
	s.f.start()
	check()
	retry := createFailedSwarm(t, s.alice, s.shared, "alice-failed")
	if retry.ID != alice.ID {
		t.Error("retry duplicated the durable failed swarm")
	}
	check()
}

func testRevocation(t *testing.T, bin binaries) {
	s := setUp(t, bin)
	f := s.f
	f.cli("alice-state", "alice\n"+alicePassword+"\n", 0, "login", f.origin)
	s.admin.expect("POST", "/api/admin/users/alice/disable", nil, 204)
	s.alice.expect("GET", "/api/projects", nil, 401)
	f.cli("alice-state", "", 2, "whoami", f.origin)
	f.cli("alice-state", "alice\n"+alicePassword+"\n", 2, "login", f.origin)
	s.admin.expect("POST", "/api/admin/users/alice/enable", nil, 204)
	// Разблокировка не воскрешает отозванные сессии.
	f.cli("alice-state", "", 2, "whoami", f.origin)
	s.alice.expect("GET", "/api/projects", nil, 401)
	f.cli("bob-state", "bob\n"+bobPassword+"\n", 0, "login", f.origin)
	newPassword := "bob-reset-acceptance-password-7731"
	f.passwords = append(f.passwords, newPassword)
	s.admin.expect("POST", "/api/admin/users/bob/password-reset", map[string]string{"new_password": newPassword}, 204)
	s.bob.expect("GET", "/api/projects", nil, 401)
	for range 2 {
		f.cli("bob-state", "", 2, "whoami", f.origin)
	}
	f.cli("bob-state", "bob\n"+bobPassword+"\n", 2, "login", f.origin)
	f.cli("bob-state", "bob\n"+newPassword+"\n", 0, "login", f.origin)
	s.bob.login("bob", newPassword)
	expectProjects(t, s.bob, s.shared)
	// Членство отзывается на каждом запросе, без отзыва всей учётной записи.
	s.admin.expect("DELETE", "/api/projects/"+s.shared.ID+"/members/bob", nil, 204)
	expectProjects(t, s.bob)
	s.bob.expect("GET", "/api/projects/"+s.shared.ID+"/swarms", nil, 403)
	s.bob.expect("POST", "/api/projects/"+s.shared.ID+"/swarms", map[string]string{"name": "removed-member-bypass"}, 403)
	expectCLIContains(t, f.cli("bob-state", "", 0, "whoami", f.origin), "bob")
	expectCLIContains(t, f.cli("bob-state", "", 0, "projects", "list", f.origin), "no projects")
}

func testNegativeCLI(t *testing.T, bin binaries) {
	f := newInstallation(t, bin)
	for _, args := range [][]string{
		{}, {"unknown-command"}, {"whoami", "--unknown"}, {"login", "--ca"},
		{"whoami", f.origin, f.origin}, {"projects", "not-a-subcommand", f.origin},
		{"help", "unexpected"}, {"login", "http://example.invalid"},
	} {
		out := f.cli("negative-state", "", 1, args...)
		if strings.TrimSpace(out) == "" {
			t.Errorf("invalid CLI args %q produced no diagnostic", args)
		}
	}
	for _, args := range [][]string{{"unknown-command"}, {"serve", "unexpected"}, {"bootstrap-admin", "unexpected"}} {
		f.run(bin.brood, "", "", 1, args...)
	}
}

func readCLIState(t *testing.T, f *installation, state string) []byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.dir, state, "brood", "sessions", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("expected exactly one saved CLI session, found %d: %v", len(paths), err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return data
}
