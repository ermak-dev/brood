//go:build linux

package guest

// BUG: Инверсия .gitignore публикует секрет, доступный обычному git add.
// HYPOTHESIS: Наличие строки в info/exclude не проверяет эффективные правила и индекс.
// SOLUTION: Read-only Git plumbing проверяет индекс и effective check-ignore
// до публикации файлов; в argv попадают только пути, .gitignore не меняется.
// RESULT: FIXED — !* и tracked пути отклонены до записи; обычный git add чист.
// STATUS: FIXED

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/projectsecret"
)

func secretTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git fixture failed: %v: %s", err, out)
	}
	return string(out)
}

func TestProjectSecretFileRequiresGitCheckout(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	_, applyErr := applyProjectSecrets(root, home, projectsecret.Bundle{Files: []projectsecret.File{{Name: "key", Data: []byte("synthetic-secret")}}})
	if _, err := os.Lstat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
		t.Fatal("delivery initialized Git metadata")
	}
	if _, err := os.Lstat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("delivery created user ignore rules")
	}
	secretTestGit(t, root, "init", "-q")
	secretTestGit(t, root, "add", "--all")
	tracked := secretTestGit(t, root, "ls-files", "-z")
	if applyErr == nil || tracked != "" {
		t.Fatalf("file delivery without Git accepted or leaked after git init/add: error=%v tracked=%q", applyErr, tracked)
	}
	if _, err := os.Stat(filepath.Join(root, "key")); !os.IsNotExist(err) {
		t.Fatal("secret bytes published without a Git checkout")
	}
}

func TestProjectSecretEnvironmentWithoutGitCheckout(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	env, err := applyProjectSecrets(root, home, projectsecret.Bundle{Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}})
	if err != nil || !strings.Contains(strings.Join(env, "\n"), "SERVICE_KEY=synthetic-env") {
		t.Fatal("environment-only delivery requires Git", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("environment-only delivery changed the project directory", err)
	}
}

func TestProjectSecretRefusesGitIgnorePayload(t *testing.T) {
	for _, dir := range []string{"", "nested"} {
		t.Run(dir, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			secretTestGit(t, root, "init", "-q")
			ignore, key := filepath.Join(dir, ".gitignore"), filepath.Join(dir, "key")
			bundle := projectsecret.Bundle{Files: []projectsecret.File{
				{Name: ignore, Data: []byte("!*\n")},
				{Name: key, Data: []byte("synthetic-secret")},
			}}
			_, applyErr := applyProjectSecrets(root, home, bundle)
			secretTestGit(t, root, "add", "--all")
			tracked := secretTestGit(t, root, "ls-files", "-z")
			if applyErr == nil || tracked != "" {
				t.Fatalf("secret ignore payload accepted or exposed secret to git add: error=%v tracked=%q", applyErr, tracked)
			}
			for _, name := range []string{ignore, key} {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatal("rejected ignore payload published files")
				}
			}
		})
	}
}

func TestProjectSecretEffectiveGitIgnore(t *testing.T) {
	for _, kind := range []string{"root-negation", "nested-negation", "tracked-missing", "tracked-owned"} {
		t.Run(kind, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			secretTestGit(t, root, "init", "-q")
			name := "key[1]"
			bundle := projectsecret.Bundle{Files: []projectsecret.File{{Name: name, Data: []byte("synthetic-first")}}}
			if kind == "tracked-owned" {
				if _, err := applyProjectSecrets(root, home, bundle); err != nil {
					t.Fatal(err)
				}
				secretTestGit(t, root, "add", "-f", "--", name)
				bundle.Files[0].Data = []byte("synthetic-second")
			} else if kind == "tracked-missing" {
				if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				secretTestGit(t, root, "add", "--", name)
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			} else {
				ignore := ".gitignore"
				if kind == "nested-negation" {
					name = "nested/key[1]"
					bundle.Files[0].Name = name
					if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
						t.Fatal(err)
					}
					ignore = "nested/.gitignore"
				}
				if err := os.WriteFile(filepath.Join(root, ignore), []byte("!*\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := applyProjectSecrets(root, home, bundle)
			if err == nil {
				status := secretTestGit(t, root, "status", "--porcelain", "--untracked-files=all")
				t.Fatalf("exposed secret accepted; real git status: %q", status)
			}
			data, readErr := os.ReadFile(filepath.Join(root, name))
			if kind == "tracked-owned" {
				if readErr != nil || string(data) != "synthetic-first" {
					t.Fatal("tracked owned file changed")
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal("secret bytes published before Git safety check")
			}
			if kind == "root-negation" || kind == "nested-negation" {
				ignore := ".gitignore"
				if kind == "nested-negation" {
					ignore = "nested/.gitignore"
				}
				data, _ := os.ReadFile(filepath.Join(root, ignore))
				if string(data) != "!*\n" {
					t.Fatal("user ignore modified")
				}
			}
		})
	}
}

func TestProjectSecretGitLiteralPatterns(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	secretTestGit(t, root, "init", "-q")
	names := []string{"key[1]", "key*?", "#key!", "space key", "nested/key[2]"}
	bundle := projectsecret.Bundle{}
	for _, name := range names {
		bundle.Files = append(bundle.Files, projectsecret.File{Name: name, Data: []byte("synthetic")})
	}
	if _, err := applyProjectSecrets(root, home, bundle); err != nil {
		t.Fatal(err)
	}
	status := secretTestGit(t, root, "status", "--porcelain", "--untracked-files=all")
	if status != "" {
		t.Fatalf("real git status exposes secrets: %q", status)
	}
	if err := os.WriteFile(filepath.Join(root, "ordinary"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	secretTestGit(t, root, "add", "--all")
	tracked := secretTestGit(t, root, "ls-files", "-z")
	if strings.Trim(tracked, "\x00") != "ordinary" {
		t.Fatal("normal git add staged secrets")
	}
}
