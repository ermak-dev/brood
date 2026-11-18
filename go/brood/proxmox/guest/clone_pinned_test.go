//go:build linux

package guest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FEATURE: Bootstrap-клон пустого workspace на точный commit, не default HEAD (#292).
// SPEC: Не публиковать default checkout или частичный clone, не переиспользовать
// живую работу, отказывать для неканонического commit до файлов и сети.
// IMPLEMENTATION: Отдельный pinned-путь выполняет detached checkout в staging.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestPinnedClonePublishesExactCommitThroughRealGateway(t *testing.T) {
	remote := newGitRemoteFixture(t)
	pin := cloneTestGit(t, remote.bare, "rev-parse", "HEAD")
	source := filepath.Join(filepath.Dir(remote.bare), "source")
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("future branch content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "future-only"), []byte("not part of the pin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cloneTestGit(t, source, "add", "README", "future-only")
	cloneTestGit(t, source, "commit", "-q", "-m", "advance default branch")
	cloneTestGit(t, source, "push", "-q", remote.bare, "HEAD:refs/heads/main")
	if current := cloneTestGit(t, remote.bare, "rev-parse", "HEAD"); current == pin {
		t.Fatal("fixture did not advance default branch")
	}
	project, home := privateTemp(t), privateTemp(t)
	if err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: remote.gateway, Timeout: 30 * time.Second, Commit: pin}); err != nil {
		t.Fatal(err)
	}
	if got := cloneTestGit(t, project, "rev-parse", "HEAD"); got != pin {
		t.Fatalf("cloned default HEAD instead of pin: %s, want %s", got, pin)
	}
	if got := cloneTestGit(t, project, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("pinned clone is not detached: %s", got)
	}
	if data, err := os.ReadFile(filepath.Join(project, "README")); err != nil || string(data) != "fixture\n" {
		t.Fatalf("wrong pinned tree: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(project, "future-only")); !os.IsNotExist(err) {
		t.Fatalf("default-only file reached workspace: %v", err)
	}
	if got := cloneTestGit(t, project, "config", "--get", "brood.origin"); got != cloneTestOrigin {
		t.Fatalf("canonical origin lost: %s", got)
	}
	if got := cloneTestGit(t, project, "config", "--get", "remote.origin.url"); got != remote.gateway {
		t.Fatalf("gateway remote lost: %s", got)
	}
	if remote.requests.Load() == 0 {
		t.Fatal("pinned clone bypassed authenticated Git gateway")
	}
}

func TestPinnedCloneRefusesMalformedCommitBeforeFilesOrGit(t *testing.T) {
	for _, pin := range []string{"HEAD", "--upload-pack=other", strings.Repeat("a", 39), strings.Repeat("A", 40), strings.Repeat("a", 64), strings.Repeat("g", 40)} {
		t.Run(pin, func(t *testing.T) {
			project, home := privateTemp(t), privateTemp(t)
			called := false
			err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: "http://127.0.0.1:8318/origin.git", Timeout: time.Second, Commit: pin, Git: func(context.Context, []string, ...string) error { called = true; return nil }})
			if err == nil || called || len(entries(t, project)) != 0 || len(entries(t, home)) != 0 {
				t.Fatalf("invalid pin reached files or git: called=%v err=%v", called, err)
			}
		})
	}
}

func TestPinnedCloneNeverReusesOrOverwritesNonemptyWorkspace(t *testing.T) {
	project, home := privateTemp(t), privateTemp(t)
	path := filepath.Join(project, "owner-work")
	if err := os.WriteFile(path, []byte("keep exactly\n"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: "http://127.0.0.1:8318/origin.git", Timeout: time.Second, Commit: strings.Repeat("a", 40), Git: func(context.Context, []string, ...string) error { called = true; return nil }})
	if err == nil || called {
		t.Fatalf("nonempty pinned workspace accepted: called=%v err=%v", called, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep exactly\n" {
		t.Fatalf("existing work changed: %q %v", data, err)
	}
}

func TestPinnedCloneUnavailableCommitPublishesNothing(t *testing.T) {
	remote := newGitRemoteFixture(t)
	project, home := privateTemp(t), privateTemp(t)
	err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: remote.gateway, Timeout: 30 * time.Second, Commit: strings.Repeat("f", 40)})
	if err == nil || len(entries(t, project)) != 0 {
		t.Fatalf("unavailable pin published partial or default checkout: %v", err)
	}
}
