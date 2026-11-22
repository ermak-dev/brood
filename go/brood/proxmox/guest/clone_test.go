//go:build linux

package guest

// FEATURE: First-start clone of the actor origin (#147).
// SPEC: The UID 1000 wrapper clones into private staging under home/.config/brood,
// straight from the origin, records
// the canonical origin there, and publishes only into an empty workspace before
// file secrets. Failed staging is removed without touching live work. Retry is
// automatic only while empty; interrupted nonempty live work requires explicit reset.
import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/projectsecret"
)

const cloneTestOrigin = "https://git.example.invalid/group/repo.git"

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
	spec := cloneSpec{Project: project, Home: home, Timeout: time.Second, Git: git}
	if err := cloneWorkspace(context.Background(), spec); err != nil || clones != 0 {
		t.Fatalf("no origin cloned: %v %d", err, clones)
	}
	spec.Origin = cloneTestOrigin
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
		cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Timeout: time.Second, Git: func(context.Context, []string, ...string) error {
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
	if err := os.WriteFile(filepath.Join(project, "work"), []byte("user\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Timeout: 30 * time.Second}); err == nil || !strings.Contains(err.Error(), "workspace preserved") {
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
	if err := cloneWorkspace(context.Background(), cloneSpec{Project: user, Home: userHome, Origin: cloneTestOrigin, Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if names := entries(t, user); !slices.Equal(names, []string{"work"}) {
		t.Fatalf("user workspace changed: %v", names)
	}
}

func TestCloneDirectToOrigin(t *testing.T) {
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
	spec := cloneSpec{Project: project, Home: home, Origin: origin, Timeout: time.Second, Git: git}
	if err := cloneWorkspace(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls: %q", calls)
	}
	clone := calls[0]
	destination := clone[len(clone)-1]
	if !slices.Equal(clone[len(clone)-4:], []string{"clone", "--", origin, destination}) || !strings.Contains(strings.Join(clone, " "), "-c credential.helper= ") || !strings.Contains(strings.Join(clone, " "), "-c http.followRedirects=false ") {
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
	if strings.Contains(strings.ToLower(env), "proxy=") {
		t.Fatalf("clone env names a proxy: %s", env)
	}
	if !slices.Contains(envs[0], "GIT_ALLOW_PROTOCOL=https") {
		t.Fatalf("direct clone protocol: %s", env)
	}
}

var cloneTestIdentity = gitidentity.Identity{Name: "brood worker2", Email: "worker2@brood.corp.example"}

// SPEC (#249): the clone records the server-assigned identity in the staged
// clone's own .git/config, after brood.origin and before publication; without
// an identity nothing else is written. HOME is never configured.
func TestCloneRecordsActorGitIdentity(t *testing.T) {
	for _, identity := range []gitidentity.Identity{cloneTestIdentity, {}} {
		project, home := privateTemp(t), privateTemp(t)
		var calls [][]string
		git := func(_ context.Context, env []string, args ...string) error {
			calls = append(calls, args)
			if slices.Contains(args, "clone") {
				return os.MkdirAll(filepath.Join(cloneTestDestination(t, args, project, home), ".git"), 0700)
			}
			return nil
		}
		spec := cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Timeout: time.Second, Git: git, Identity: identity}
		if err := cloneWorkspace(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		if identity.IsZero() {
			if len(calls) != 2 {
				t.Fatalf("without identity: %q", calls)
			}
			continue
		}
		if len(calls) != 4 {
			t.Fatalf("with identity: %q", calls)
		}
		destination := calls[0][len(calls[0])-1]
		for i, want := range [][]string{{"-C", destination, "config", "user.name", identity.Name}, {"-C", destination, "config", "user.email", identity.Email}} {
			got := calls[2+i]
			if !slices.Equal(got[len(got)-5:], want) || slices.Contains(got, project) || slices.Contains(got, "--global") {
				t.Fatalf("identity config %d: %q", i, got)
			}
		}
		if _, err := os.Lstat(filepath.Join(home, ".gitconfig")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("identity written to home")
		}
	}

}

func TestWorkspaceOriginProjection(t *testing.T) {
	dir := privateTemp(t)
	publish := func(origin string) error {
		return publishWorkspaceOrigin(dir, WorkspaceOrigin{Origin: origin}, os.Geteuid(), os.Getegid())
	}
	if err := publish(cloneTestOrigin); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dir, workspaceOriginFile))
	if err != nil || info.Mode().Perm() != 0440 {
		t.Fatalf("projection mode: %v %v", info, err)
	}
	got, err := readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid())
	if err != nil || got != (WorkspaceOrigin{Origin: cloneTestOrigin}) {
		t.Fatalf("read back: %+v %v", got, err)
	}
	// Идентичность (#249) едет в той же проекции и читается обратно целиком.
	identified := WorkspaceOrigin{Origin: cloneTestOrigin, GitIdentity: cloneTestIdentity}
	if err := publishWorkspaceOrigin(dir, identified, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if got, err = readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err != nil || got != identified {
		t.Fatalf("identity read back: %+v %v", got, err)
	}
	for _, invalid := range []WorkspaceOrigin{
		{GitIdentity: cloneTestIdentity},
		{Origin: cloneTestOrigin, GitIdentity: gitidentity.Identity{Name: "brood <x>", Email: cloneTestIdentity.Email}},
		{Origin: cloneTestOrigin, GitIdentity: gitidentity.Identity{Name: cloneTestIdentity.Name}},
	} {
		if err := publishWorkspaceOrigin(dir, invalid, os.Geteuid(), os.Getegid()); err == nil {
			t.Errorf("invalid identity published: %+v", invalid)
		}
	}
	if err := publish(""); err != nil {
		t.Fatal(err)
	}
	if got, err = readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err != nil || got != (WorkspaceOrigin{}) {
		t.Fatalf("cleared origin: %+v %v", got, err)
	}
	path := filepath.Join(dir, workspaceOriginFile)
	os.Remove(path)
	os.WriteFile(path, []byte(`{"origin":"`+cloneTestOrigin+`","extra":1}`), 0440)
	if _, err := readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("unknown projection field accepted")
	}
	os.Remove(path)
	os.Symlink(filepath.Join(t.TempDir(), "target"), path)
	if err := publishWorkspaceOrigin(dir, WorkspaceOrigin{Origin: cloneTestOrigin}, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("projection symlink replaced")
	}
	if _, err := readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("projection symlink read")
	}
}

func TestProjectAppStartsWithEmptyWorkspaceAfterCloneFailure(t *testing.T) {
	run, project, home := privateTemp(t), privateTemp(t), privateTemp(t)
	bundle := projectsecret.Bundle{Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}, Files: []projectsecret.File{{Name: "key", Data: []byte("synthetic-file")}}}
	if err := publishProjectSecrets(run, bundle, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkspaceOrigin(run, WorkspaceOrigin{Origin: cloneTestOrigin}, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	layout := appLayout{Project: project, Home: home, Run: run, RunUID: os.Geteuid(), RunGID: os.Getegid(), CloneTimeout: time.Second}
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
