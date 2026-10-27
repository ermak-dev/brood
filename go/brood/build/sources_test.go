// Снимок commit не переносит настройки оператора и ссылки наружу.
// Порт brood/dev/build/tests/test_sources.py.
package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitFixture обрамление тестового репозитория, как GitCase в Python.
type gitFixture struct {
	t    *testing.T
	root string
	repo string
	sha  string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &gitFixture{t: t, root: root, repo: repo}
	f.git("init", "-q", "--template=")
	f.git("config", "user.email", "fixture@example.invalid")
	f.git("config", "user.name", "Fixture")
	f.git("remote", "add", "origin", "https://example.org/project.git")
	f.write(".gitignore", ".env\nsecrets/\n.brood/\n")
	f.write("code.py", "version = 1\n")
	f.commit()
	return f
}

func (f *gitFixture) git(arguments ...string) string {
	f.t.Helper()
	command := exec.Command("git", append([]string{"-C", f.repo}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *gitFixture) commit() {
	f.t.Helper()
	f.git("add", ".")
	f.git("commit", "-qm", "fixture")
	f.sha = f.git("rev-parse", "HEAD")
}

func (f *gitFixture) write(relative, text string) {
	f.t.Helper()
	path := filepath.Join(f.repo, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gitFixture) read(t *testing.T, root, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func wantError(t *testing.T, err error, part string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), part) {
		t.Fatalf("expected error containing %q, got %v", part, err)
	}
}

func wantNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSnapshotUsesCommitAndExcludesLocalSettings(t *testing.T) {
	f := newGitFixture(t)
	f.write("code.py", "dirty\n")
	f.write(".env", "TOKEN=private")
	f.write("untracked", "not part of commit")
	target := filepath.Join(f.root, "snapshot")
	report, err := Snapshot(f.repo, f.sha, target, false)
	wantNoError(t, err)
	if report["revision"] != f.sha {
		t.Fatalf("revision: %v", report["revision"])
	}
	if got := f.read(t, target, "code.py"); got != "version = 1\n" {
		t.Fatalf("code.py: %q", got)
	}
	for _, name := range []string{".env", "untracked", ".git"} {
		if _, err := os.Lstat(filepath.Join(target, name)); err == nil {
			t.Fatalf("%s must not exist in the snapshot", name)
		}
	}
}

func TestGitMetadataPreservesRevisionNotLocalConfigOrPaths(t *testing.T) {
	f := newGitFixture(t)
	f.git("config", "credential.helper", "private-helper")
	target := filepath.Join(f.root, "snapshot")
	_, err := Snapshot(f.repo, f.sha, target, true)
	wantNoError(t, err)
	output, err := Git(target, "rev-parse", "HEAD")
	wantNoError(t, err)
	if strings.TrimSpace(output) != f.sha {
		t.Fatalf("HEAD: %q", output)
	}
	config := f.read(t, target, ".git/config")
	if strings.Contains(config, "private-helper") || strings.Contains(config, f.repo) {
		t.Fatalf("config leaks local settings or paths:\n%s", config)
	}
	if _, err := os.Stat(filepath.Join(target, ".git", "FETCH_HEAD")); err == nil {
		t.Fatal("FETCH_HEAD must be removed")
	}
}

func TestRepeatedGitMetadataIsByteStable(t *testing.T) {
	f := newGitFixture(t)
	first := filepath.Join(f.root, "first")
	second := filepath.Join(f.root, "second")
	_, err := Snapshot(f.repo, f.sha, first, true)
	wantNoError(t, err)
	_, err = Snapshot(f.repo, f.sha, second, true)
	wantNoError(t, err)
	if err := equalTrees(filepath.Join(first, ".git"), filepath.Join(second, ".git")); err != nil {
		t.Fatal(err)
	}
	output, err := Git(first, "status", "--porcelain")
	wantNoError(t, err)
	if output != "" {
		t.Fatalf("status must be clean, got %q", output)
	}
}

// equalTrees сравнивает все файлы двух каталогов побайтово.
func equalTrees(left, right string) error {
	collect := func(root string) (map[string]string, error) {
		files := map[string]string{}
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[relative] = string(data)
			return nil
		})
		return files, err
	}
	first, err := collect(left)
	if err != nil {
		return err
	}
	second, err := collect(right)
	if err != nil {
		return err
	}
	for name, data := range first {
		other, ok := second[name]
		if !ok || other != data {
			return fmt.Errorf(".git differs at %s", name)
		}
	}
	if len(first) != len(second) {
		return fmt.Errorf(".git file counts differ: %d vs %d", len(first), len(second))
	}
	return nil
}

func TestTrackedSecretIsRefusedBeforeExport(t *testing.T) {
	f := newGitFixture(t)
	f.write(".env", "TOKEN=private")
	f.git("add", "-f", ".env")
	f.git("commit", "-qm", "fixture secret")
	sha := f.git("rev-parse", "HEAD")
	target := filepath.Join(f.root, "snapshot")
	_, err := Snapshot(f.repo, sha, target, false)
	wantError(t, err, "secret")
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("target must not appear")
	}
}

func TestVendoredDotenvFixtureIsNotInstallationSetting(t *testing.T) {
	f := newGitFixture(t)
	f.write("vendor/dotenv/.env", "HELLO=world\n")
	f.git("add", "-f", "vendor/dotenv/.env")
	f.commit()
	target := filepath.Join(f.root, "snapshot")
	_, err := Snapshot(f.repo, f.sha, target, false)
	wantNoError(t, err)
	if got := f.read(t, target, "vendor/dotenv/.env"); got != "HELLO=world\n" {
		t.Fatalf("vendored .env: %q", got)
	}
}

func TestEscapeSymlinkIsRefused(t *testing.T) {
	f := newGitFixture(t)
	if err := os.Symlink("../private", filepath.Join(f.repo, "escape")); err != nil {
		t.Fatal(err)
	}
	f.commit()
	_, err := Snapshot(f.repo, f.sha, filepath.Join(f.root, "snapshot"), false)
	wantError(t, err, "symlink")
}

func TestCredentialsInOriginAreNeverEchoed(t *testing.T) {
	f := newGitFixture(t)
	f.git("remote", "set-url", "origin", "https://user:private-token@example.org/project.git")
	_, err := Snapshot(f.repo, f.sha, filepath.Join(f.root, "snapshot"), true)
	if err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("expected repository refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "private-token") {
		t.Fatalf("error discloses credentials: %v", err)
	}
}

func TestExistingDestinationIsNotChanged(t *testing.T) {
	f := newGitFixture(t)
	target := filepath.Join(f.root, "snapshot")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "mine"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Snapshot(f.repo, f.sha, target, false)
	wantError(t, err, "exists")
	if got := f.read(t, target, "mine"); got != "keep" {
		t.Fatalf("mine: %q", got)
	}
}
