//go:build linux

package guest

// FEATURE: First-start clone of the actor origin (#147).
// SPEC: The UID 1000 wrapper clones into private staging under home/.config/brood,
// through the local Git gateway (gateway) or the egress proxy (direct), records
// the canonical origin there, and publishes only into an empty workspace before
// file secrets. Failed staging is removed without touching live work. Retry is
// automatic only while empty; interrupted nonempty live work requires explicit reset.
// IMPLEMENTATION: Real Git through the gateway to git http-backend (CGI)
// behind an httptest TLS server standing in for Brood; fake runners for argv.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/projectsecret"
)

const cloneTestOrigin = "https://git.example.org/group/repo.git"

func cloneTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type gitRemoteFixture struct {
	bare     string
	gateway  string
	requests atomic.Int32
}

// newGitRemoteFixture serves a bare repository with git http-backend behind a
// fake Brood server, reachable only through a real local Git gateway.
func newGitRemoteFixture(t *testing.T) *gitRemoteFixture {
	t.Helper()
	root := t.TempDir()
	f := &gitRemoteFixture{bare: filepath.Join(root, "repo.git")}
	source := filepath.Join(root, "source")
	cloneTestGit(t, root, "init", "-q", "--initial-branch=main", source)
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cloneTestGit(t, source, "add", "README")
	cloneTestGit(t, source, "commit", "-q", "-m", "fixture")
	cloneTestGit(t, root, "clone", "-q", "--bare", source, f.bare)
	cloneTestGit(t, f.bare, "config", "http.receivepack", "true")
	backend := &cgi.Handler{Path: "/usr/bin/git", Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, ok := strings.CutPrefix(r.URL.Path, "/api/proxmox/git/actor/1/")
		if !ok || r.Header.Get("Authorization") != "Bearer "+gitTestToken || r.Header.Get("Cookie") != "" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		f.requests.Add(1)
		r.Header.Del("Authorization")
		r.URL.Path = "/repo.git/" + op
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	var handler http.Handler
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(front.Close)
	var err error
	if handler, err = newGitGateway(gitTestConfig(server), front.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	f.gateway = front.URL + "/origin.git"
	return f
}

func deadGateway(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return "http://" + address + "/origin.git"
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

func TestCloneThroughGitGatewayAndPushBack(t *testing.T) {
	f := newGitRemoteFixture(t)
	project, home := privateTemp(t), privateTemp(t)
	spec := cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: f.gateway, Timeout: 30 * time.Second}
	if err := cloneWorkspace(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(project, "README")); err != nil || string(data) != "fixture\n" {
		t.Fatalf("clone content: %q %v", data, err)
	}
	if got := cloneTestGit(t, project, "config", "--get", "remote.origin.url"); got != f.gateway {
		t.Fatalf("remote origin %q, want the clone URL %q", got, f.gateway)
	}
	if got := cloneTestGit(t, project, "config", "--get", "brood.origin"); got != cloneTestOrigin {
		t.Fatalf("brood.origin %q", got)
	}
	if err := os.WriteFile(filepath.Join(project, "change"), []byte("pushed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cloneTestGit(t, project, "add", "change")
	cloneTestGit(t, project, "commit", "-q", "-m", "change")
	cloneTestGit(t, project, "push", "-q", "origin", "HEAD:refs/heads/feature")
	if got, want := cloneTestGit(t, f.bare, "rev-parse", "refs/heads/feature"), cloneTestGit(t, project, "rev-parse", "HEAD"); got != want {
		t.Fatalf("push through gateway: %s want %s", got, want)
	}
	before := f.requests.Load()
	if err := cloneWorkspace(context.Background(), spec); err != nil || f.requests.Load() != before {
		t.Fatalf("existing clone touched on next start: %v", err)
	}
}

func TestCloneFailureIsBoundedAndRetriedWhileEmpty(t *testing.T) {
	project, home := privateTemp(t), privateTemp(t)
	spec := cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: deadGateway(t), Timeout: 30 * time.Second}
	err := cloneWorkspace(context.Background(), spec)
	if err == nil || len(err.Error()) > 512 || !strings.Contains(err.Error(), "clone") || strings.ContainsAny(err.Error(), "\r\n\x1b") {
		t.Fatalf("unbounded or missing clone failure: %q", err)
	}
	if names := entries(t, project); len(names) != 0 {
		t.Fatalf("failed clone left %v", names)
	}
	spec.Gateway = newGitRemoteFixture(t).gateway
	if err := cloneWorkspace(context.Background(), spec); err != nil {
		t.Fatalf("retry on next start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "README")); err != nil {
		t.Fatal("retry did not clone")
	}
}

func cloneTestDestination(t *testing.T, args []string, project, home string) string {
	t.Helper()
	// Путь /proc/<pid>/fd может закреплять безопасно открытый каталог;
	// проверяем реальное размещение, а не текстовый префикс argv.
	destination, err := filepath.EvalSymlinks(args[len(args)-1])
	if err != nil {
		t.Fatalf("resolve clone staging destination: %v", err)
	}
	rel, err := filepath.Rel(filepath.Join(home, ".config", "brood"), destination)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || destination == project {
		t.Fatalf("clone destination is not isolated private staging: %q", destination)
	}
	return destination
}

func TestCloneFailureRemovesOnlyStageAndPreservesSubsequentUserWork(t *testing.T) {
	project, home := privateTemp(t), privateTemp(t)
	var clones int
	var stage string
	git := func(_ context.Context, _ []string, args ...string) error {
		if !slices.Contains(args, "clone") {
			return nil
		}
		clones++
		stage = cloneTestDestination(t, args, project, home)
		if names := entries(t, project); len(names) != 0 {
			t.Errorf("clone started with a non-empty workspace: %v", names)
		}
		if err := os.MkdirAll(filepath.Join(stage, ".git", "objects"), 0700); err != nil {
			t.Fatal(err)
		}
		// Git, убитый SIGKILL, оставляет частичный клон только в staging.
		return errors.New("killed")
	}
	spec := cloneSpec{Project: project, Home: home, Gateway: deadGateway(t), Timeout: time.Second, Git: git}
	if err := cloneWorkspace(context.Background(), spec); err != nil || clones != 0 {
		t.Fatalf("no origin cloned: %v %d", err, clones)
	}
	spec.Origin, spec.Mode = cloneTestOrigin, "gateway"
	if err := cloneWorkspace(context.Background(), spec); err == nil || clones != 1 {
		t.Fatalf("failed clone not reported: %v %d", err, clones)
	}
	if names := entries(t, project); len(names) != 0 {
		t.Fatalf("failed staging clone polluted the workspace: %v", names)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed clone stage was not removed: %v", err)
	}
	// Работа после неудачного клона — не staging: её не трогают.
	if err := os.WriteFile(filepath.Join(project, "work"), []byte("user\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cloneWorkspace(context.Background(), spec); err != nil || clones != 1 {
		t.Fatalf("user work after a failed clone was cloned over: %v %d", err, clones)
	}
	if data, err := os.ReadFile(filepath.Join(project, "work")); err != nil || string(data) != "user\n" {
		t.Fatal("user work changed")
	}
}

// killedCloneEnv makes the test binary act as a wrapper killed mid-clone.
const killedCloneEnv = "BROOD_TEST_KILLED_CLONE"

func TestCloneResidueOfKilledWrapperIsPreserved(t *testing.T) {
	if paths := os.Getenv(killedCloneEnv); paths != "" {
		project, home, _ := strings.Cut(paths, "\n")
		cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: "http://127.0.0.1:8318/origin.git", Timeout: time.Second, Git: func(context.Context, []string, ...string) error {
			// Намеренно моделируем остаток в live, а не в argv staging:
			// прерванная публикация или старый wrapper оставили pending и .git.
			os.MkdirAll(filepath.Join(project, ".git", "objects"), 0700)
			os.Exit(3) // SIGKILL от stop: wrapper не снимает pending
			return nil
		}})
		os.Exit(0)
	}
	project, home := privateTemp(t), privateTemp(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCloneResidueOfKilledWrapperIsPreserved$")
	cmd.Env = append(os.Environ(), killedCloneEnv+"="+project+"\n"+home)
	if err := cmd.Run(); err == nil {
		t.Fatal("helper wrapper was not killed mid-clone")
	}
	if names := entries(t, project); len(names) == 0 {
		t.Fatal("killed clone left no residue to recover")
	}
	f := newGitRemoteFixture(t)
	if err := os.WriteFile(filepath.Join(project, "work"), []byte("user\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: f.gateway, Timeout: 30 * time.Second}); err == nil || !strings.Contains(err.Error(), "workspace preserved") {
		t.Fatalf("interrupted clone must require explicit recovery: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(project, "work")); err != nil || string(data) != "user\n" {
		t.Fatalf("work after interruption changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(project, ".git", "objects")); err != nil {
		t.Fatal("interrupted clone was removed")
	}
	user, userHome := privateTemp(t), privateTemp(t)
	os.WriteFile(filepath.Join(user, "work"), []byte("user\n"), 0600)
	if err := cloneWorkspace(context.Background(), cloneSpec{Project: user, Home: userHome, Origin: cloneTestOrigin, Mode: "gateway", Gateway: f.gateway, Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if names := entries(t, user); !slices.Equal(names, []string{"work"}) {
		t.Fatalf("user workspace changed: %v", names)
	}
}

func TestCloneRoutesGatewayLocallyAndDirectThroughEgressProxy(t *testing.T) {
	for _, mode := range []string{"gateway", "direct"} {
		t.Run(mode, func(t *testing.T) {
			project, home := privateTemp(t), privateTemp(t)
			var calls [][]string
			var envs [][]string
			var stage string
			git := func(_ context.Context, env []string, args ...string) error {
				calls, envs = append(calls, args), append(envs, env)
				if slices.Contains(args, "clone") {
					stage = cloneTestDestination(t, args, project, home)
					return os.MkdirAll(filepath.Join(stage, ".git"), 0700)
				}
				return nil
			}
			origin := "https://github.com/owner/repo"
			spec := cloneSpec{Project: project, Home: home, Origin: origin, Mode: mode, Gateway: "http://127.0.0.1:8318/origin.git", Timeout: time.Second, Git: git}
			if err := cloneWorkspace(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 {
				t.Fatalf("calls: %q", calls)
			}
			url := spec.Gateway
			if mode == "direct" {
				url = origin
			}
			clone := calls[0]
			destination := clone[len(clone)-1]
			if !slices.Equal(clone[len(clone)-4:], []string{"clone", "--", url, destination}) || !strings.Contains(strings.Join(clone, " "), "-c credential.helper= ") || !strings.Contains(strings.Join(clone, " "), "-c http.followRedirects=false ") {
				t.Fatalf("clone argv: %q", clone)
			}
			config := calls[1]
			// Конфигурация должна попасть в тот же staging до публикации .git.
			if !slices.Equal(config[len(config)-5:], []string{"-C", destination, "config", "brood.origin", origin}) || slices.Contains(config, project) {
				t.Fatalf("config does not target the clone stage: %q", config)
			}
			if names := entries(t, project); !slices.Equal(names, []string{".git"}) {
				t.Fatalf("successful stage was not published: %v", names)
			}
			if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published stage was not removed: %v", err)
			}
			env := strings.Join(envs[0], "\n")
			for _, want := range []string{"HOME=" + home, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"} {
				if !strings.Contains(env, want) {
					t.Errorf("clone env misses %s: %s", want, env)
				}
			}
			proxied := strings.Contains(env, "HTTPS_PROXY=http://127.0.0.1:3128") && strings.Contains(env, "https_proxy=http://127.0.0.1:3128")
			if mode == "direct" && (!proxied || !slices.Contains(envs[0], "GIT_ALLOW_PROTOCOL=https")) {
				t.Fatalf("direct clone bypasses the egress proxy: %s", env)
			}
			if mode == "gateway" && (strings.Contains(strings.ToLower(env), "proxy=http") || !slices.Contains(envs[0], "GIT_ALLOW_PROTOCOL=http")) {
				t.Fatalf("gateway clone leaves loopback: %s", env)
			}
		})
	}
}

func TestWorkspaceOriginProjection(t *testing.T) {
	dir := privateTemp(t)
	if err := publishWorkspaceOrigin(dir, cloneTestOrigin, "gateway", os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dir, workspaceOriginFile))
	if err != nil || info.Mode().Perm() != 0440 {
		t.Fatalf("projection mode: %v %v", info, err)
	}
	got, err := readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid())
	if err != nil || got.Origin != cloneTestOrigin || got.Mode != "gateway" {
		t.Fatalf("read back: %+v %v", got, err)
	}
	if err := publishWorkspaceOrigin(dir, "", "", os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if got, err = readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err != nil || got.Origin != "" || got.Mode != "" {
		t.Fatalf("cleared origin: %+v %v", got, err)
	}
	for _, pair := range [][2]string{{cloneTestOrigin, ""}, {"", "direct"}, {"http://git.example.org/repo.git", "gateway"}, {cloneTestOrigin, "ssh"}} {
		if err := publishWorkspaceOrigin(dir, pair[0], pair[1], os.Geteuid(), os.Getegid()); err == nil {
			t.Errorf("invalid pair published: %q", pair)
		}
	}
	path := filepath.Join(dir, workspaceOriginFile)
	os.Remove(path)
	os.WriteFile(path, []byte(`{"origin":"`+cloneTestOrigin+`","mode":"gateway","extra":1}`), 0440)
	if _, err := readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("unknown projection field accepted")
	}
	os.Remove(path)
	os.Symlink(filepath.Join(t.TempDir(), "target"), path)
	if err := publishWorkspaceOrigin(dir, cloneTestOrigin, "gateway", os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("projection symlink replaced")
	}
	if _, err := readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("projection symlink read")
	}
}

func TestProjectAppClonesBeforeApplyingFileSecrets(t *testing.T) {
	f := newGitRemoteFixture(t)
	run, project, home := privateTemp(t), privateTemp(t), privateTemp(t)
	bundle := projectsecret.Bundle{Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}, Files: []projectsecret.File{{Name: "config/service.json", Data: []byte("synthetic-file")}}}
	if err := publishProjectSecrets(run, bundle, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkspaceOrigin(run, cloneTestOrigin, "gateway", os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	layout := appLayout{Project: project, Home: home, Run: run, RunUID: os.Geteuid(), RunGID: os.Getegid(), Gateway: f.gateway, CloneTimeout: 30 * time.Second}
	var warnings bytes.Buffer
	env, err := prepareProjectApp(layout, &warnings)
	if err != nil || warnings.Len() != 0 {
		t.Fatalf("fresh actor with file secrets: %v %q", err, warnings.String())
	}
	if !slices.Contains(env, "SERVICE_KEY=synthetic-env") {
		t.Fatal("secret variable missing")
	}
	if data, err := os.ReadFile(filepath.Join(project, "config/service.json")); err != nil || string(data) != "synthetic-file" {
		t.Fatalf("file secret not delivered into the fresh clone: %q %v", data, err)
	}
	if status := cloneTestGit(t, project, "status", "--porcelain"); status != "" {
		t.Fatalf("delivered secret is visible to Git: %q", status)
	}
}

func TestProjectAppStartsWithEmptyWorkspaceAfterCloneFailure(t *testing.T) {
	run, project, home := privateTemp(t), privateTemp(t), privateTemp(t)
	bundle := projectsecret.Bundle{Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}, Files: []projectsecret.File{{Name: "key", Data: []byte("synthetic-file")}}}
	if err := publishProjectSecrets(run, bundle, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkspaceOrigin(run, cloneTestOrigin, "gateway", os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	layout := appLayout{Project: project, Home: home, Run: run, RunUID: os.Geteuid(), RunGID: os.Getegid(), Gateway: deadGateway(t), CloneTimeout: 30 * time.Second}
	var warnings bytes.Buffer
	env, err := prepareProjectApp(layout, &warnings)
	if err != nil || !slices.Contains(env, "SERVICE_KEY=synthetic-env") {
		t.Fatalf("Claude must still start after a clone failure: %v", err)
	}
	text := warnings.String()
	if !strings.Contains(text, "clone") || !strings.Contains(text, "next start") || len(text) > 2048 || strings.Contains(text, "synthetic") {
		t.Fatalf("clone failure not reported safely: %q", text)
	}
	if names := entries(t, project); len(names) != 0 {
		t.Fatalf("file secret published without a checkout: %v", names)
	}
}
