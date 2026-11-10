//go:build linux

package sessionctl

// FEATURE: Pinned workspace checkout for attestation (#183).
// SPEC: Bring a clean long-lived clone to an exact commit; refuse and preserve
// local changes; never trust Git's index or object store without rehashing.
// IMPLEMENTATION: Content-based cleanliness with a temporary index, fetch from a
// root-chosen source, detached checkout without overwriting ignored files, and
// self-hashed snapshot objects.
// RESULT: IMPLEMENTED with real Git effects confined to temporary fixtures.

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type checkoutFixture struct {
	upstream, root string
	home           string
	first, second  string
	engine         *checkoutEngine
}

func fixtureEnv(home string) []string {
	return []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
}

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("/usr/bin/git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "protocol.file.allow=always"}, args...)...)
	c.Dir = dir
	c.Env = fixtureEnv(dir)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("fixture git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// newCheckoutFixture clones upstream at its first commit, then advances
// upstream, so the second commit exists only at the fetch source.
func newCheckoutFixture(t *testing.T) *checkoutFixture {
	return newCheckoutFixtureWith(t, nil)
}

// newCheckoutFixtureWith adds files to the first commit. The clone is a plain
// one, like the first-start clone, so in-tree .gitattributes apply to it.
func newCheckoutFixtureWith(t *testing.T, files map[string]string) *checkoutFixture {
	t.Helper()
	base := t.TempDir()
	f := &checkoutFixture{upstream: filepath.Join(base, "upstream"), root: filepath.Join(base, "project")}
	if err := os.Mkdir(f.upstream, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, f.upstream, "init", "--quiet", "--initial-branch=main")
	writeFile(t, filepath.Join(f.upstream, "tracked"), "first\n")
	writeFile(t, filepath.Join(f.upstream, ".gitignore"), "*.log\n")
	for name, data := range files {
		writeFile(t, filepath.Join(f.upstream, name), data)
	}
	fixtureGit(t, f.upstream, "add", "--all")
	fixtureGit(t, f.upstream, "commit", "--quiet", "-m", "first")
	f.first = fixtureGit(t, f.upstream, "rev-parse", "HEAD")
	fixtureGit(t, base, "clone", "--quiet", "--", f.upstream, f.root)
	writeFile(t, filepath.Join(f.upstream, "tracked"), "second\n")
	writeFile(t, filepath.Join(f.upstream, "dir", "new"), "new\n")
	fixtureGit(t, f.upstream, "add", "tracked", "dir/new")
	fixtureGit(t, f.upstream, "commit", "--quiet", "-m", "second")
	f.second = fixtureGit(t, f.upstream, "rev-parse", "HEAD")
	f.home = filepath.Join(base, "home")
	if err := os.Mkdir(f.home, 0700); err != nil {
		t.Fatal(err)
	}
	f.engine = &checkoutEngine{root: f.root, home: f.home, scratch: t.TempDir(), source: f.upstream, protocol: "file"}
	return f
}

func (f *checkoutFixture) head(t *testing.T) string {
	t.Helper()
	return readFile(t, filepath.Join(f.root, ".git", "HEAD"))
}

func (f *checkoutFixture) has(t *testing.T, commit string) bool {
	c := exec.Command("/usr/bin/git", "cat-file", "-e", commit+"^{commit}")
	c.Dir = f.root
	c.Env = fixtureEnv(f.root)
	return c.Run() == nil
}

func TestCheckoutBringsCleanWorkspaceToCommit(t *testing.T) {
	f := newCheckoutFixture(t)
	facts, err := f.engine.run(context.Background(), f.second)
	if err != nil || facts != (CheckoutFacts{Commit: f.second, Fetched: true}) {
		t.Fatalf("checkout: %+v %v", facts, err)
	}
	if got := f.head(t); got != f.second+"\n" {
		t.Fatalf("HEAD is not detached at the commit: %q", got)
	}
	if readFile(t, filepath.Join(f.root, "tracked")) != "second\n" || readFile(t, filepath.Join(f.root, "dir", "new")) != "new\n" {
		t.Fatal("worktree does not match the commit")
	}
	if branch := fixtureGit(t, f.root, "rev-parse", "refs/heads/main"); branch != f.first {
		t.Fatalf("local branch moved: %s", branch)
	}
	facts, err = f.engine.run(context.Background(), f.second)
	if err != nil || facts != (CheckoutFacts{Commit: f.second}) {
		t.Fatalf("repeated checkout: %+v %v", facts, err)
	}
	// Ранее полученный commit не требует сети: источник может быть недоступен.
	f.engine.source = filepath.Join(t.TempDir(), "missing")
	if _, err := f.engine.run(context.Background(), f.first); err != nil {
		t.Fatalf("local commit: %v", err)
	}
}

func TestCheckoutRefusesLocalChangesAndPreservesThem(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, f *checkoutFixture) string{
		"tracked": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.root, "tracked"), "local\n")
			return "tracked"
		},
		"untracked": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.root, "notes.txt"), "local\n")
			return "notes.txt"
		},
		"staged": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.root, "tracked"), "local\n")
			fixtureGit(t, f.root, "add", "tracked")
			return "tracked"
		},
		"assume-unchanged": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.root, "tracked"), "local\n")
			fixtureGit(t, f.root, "update-index", "--assume-unchanged", "tracked")
			return "tracked"
		},
		"skip-worktree": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.root, "tracked"), "local\n")
			fixtureGit(t, f.root, "update-index", "--skip-worktree", "tracked")
			return "tracked"
		},
		"info-exclude": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.root, "tracked"), "local\n")
			writeFile(t, filepath.Join(f.root, ".git", "info", "exclude"), "tracked\n")
			return "tracked"
		},
		// Файлы excludes и attributes по умолчанию лежат в HOME актора и
		// читаются даже без глобальной конфигурации.
		"global-excludes": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.home, ".config", "git", "ignore"), "notes.txt\n")
			writeFile(t, filepath.Join(f.root, "notes.txt"), "local\n")
			return "notes.txt"
		},
		"global-attributes": func(t *testing.T, f *checkoutFixture) string {
			writeFile(t, filepath.Join(f.home, ".config", "git", "attributes"), "* text\n")
			writeFile(t, filepath.Join(f.root, "tracked"), "first\r\n")
			return "tracked"
		},
		// Разрешённые ключи конфигурации репозитория не прячут изменений.
		"repository-filemode": func(t *testing.T, f *checkoutFixture) string {
			fixtureGit(t, f.root, "config", "core.filemode", "false")
			if err := os.Chmod(filepath.Join(f.root, "tracked"), 0700); err != nil {
				t.Fatal(err)
			}
			return "tracked"
		},
		"repository-ignorecase": func(t *testing.T, f *checkoutFixture) string {
			fixtureGit(t, f.root, "config", "core.ignorecase", "true")
			writeFile(t, filepath.Join(f.root, "TRACKED"), "local\n")
			return "TRACKED"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCheckoutFixture(t)
			path := change(t, f)
			before := readFile(t, filepath.Join(f.root, path))
			_, err := f.engine.run(context.Background(), f.second)
			if err == nil || !strings.Contains(err.Error(), "nothing changed") {
				t.Fatalf("dirty workspace accepted: %v", err)
			}
			if f.head(t) != "ref: refs/heads/main\n" || f.has(t, f.second) {
				t.Fatal("refusal changed HEAD or fetched")
			}
			if readFile(t, filepath.Join(f.root, path)) != before {
				t.Fatal("local change was not preserved")
			}
		})
	}
}

// FEATURE: Attestation compares the bytes of the commit (#183).
// SPEC: .gitattributes of the tree itself must not make an edit look clean:
// on checkin "$Id: ... $" collapses to "$Id$" and CRLF becomes LF, so Git's
// own status calls such an edit unchanged.
func TestCheckoutSeesEditsHiddenByTreeAttributes(t *testing.T) {
	for name, c := range map[string]struct{ attributes, path, committed, edited string }{
		"ident":    {"*.sh ident\n", "run.sh", "echo \"$Id$\"\n", "echo \"$Id: `touch pwned` $\"\n"},
		"text":     {"*.txt text\n", "notes.txt", "one\ntwo\n", "one\r\ntwo\r\n"},
		"eol=crlf": {"*.bat text eol=crlf\n", "run.bat", "one\ntwo\n", "one\r\ntwo\n"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCheckoutFixtureWith(t, map[string]string{".gitattributes": c.attributes, c.path: c.committed})
			writeFile(t, filepath.Join(f.root, c.path), c.edited)
			_, err := f.engine.run(context.Background(), f.second)
			if err == nil || !strings.Contains(err.Error(), "nothing changed") {
				t.Fatalf("edit hidden by %q accepted: %v", c.attributes, err)
			}
			if f.head(t) != "ref: refs/heads/main\n" || f.has(t, f.second) {
				t.Fatal("refusal changed HEAD or fetched")
			}
			if readFile(t, filepath.Join(f.root, c.path)) != c.edited {
				t.Fatal("local change was not preserved")
			}
		})
	}
}

// SPEC: a text=auto project is not converted on Linux checkout and attests.
// A clone that checkout conversions rewrote (ident, eol=crlf) differs from
// the commit bytes and is refused before any effect, naming the comparison.
// A planted object named like the empty tree does not become the attribute
// source: Git serves its built-in empty tree first.
func TestCheckoutComparesCloneBytes(t *testing.T) {
	t.Run("text=auto", func(t *testing.T) {
		f := newCheckoutFixtureWith(t, map[string]string{".gitattributes": "* text=auto\n", "notes.txt": "one\ntwo\n"})
		if _, err := f.engine.run(context.Background(), f.second); err != nil {
			t.Fatalf("normalized text project refused: %v", err)
		}
	})
	for name, files := range map[string]map[string]string{
		"ident":    {".gitattributes": "*.sh ident\n", "run.sh": "echo \"$Id$\"\n"},
		"eol=crlf": {".gitattributes": "*.bat text eol=crlf\n", "run.bat": "one\ntwo\n"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCheckoutFixtureWith(t, files)
			_, err := f.engine.run(context.Background(), f.second)
			if err == nil || !strings.Contains(err.Error(), "byte for byte") || !strings.Contains(err.Error(), "nothing changed") {
				t.Fatalf("converted clone: %v", err)
			}
			if f.head(t) != "ref: refs/heads/main\n" {
				t.Fatal("HEAD changed")
			}
		})
	}
	t.Run("planted empty tree", func(t *testing.T) {
		f := newCheckoutFixture(t)
		attributes := filepath.Join(t.TempDir(), "attributes")
		writeFile(t, attributes, "* text\n")
		blob, err := hex.DecodeString(fixtureGit(t, f.root, "hash-object", "-w", "--", attributes))
		if err != nil {
			t.Fatal(err)
		}
		writeLooseObject(t, f.root, emptyTree, "tree", append([]byte("100644 .gitattributes\x00"), blob...))
		writeFile(t, filepath.Join(f.root, "tracked"), "first\r\n")
		if _, err := f.engine.run(context.Background(), f.second); err == nil || !strings.Contains(err.Error(), "nothing changed") {
			t.Fatalf("planted attribute source hid an edit: %v", err)
		}
	})
}

func TestCheckoutKeepsIgnoredFilesOutsideTheClaim(t *testing.T) {
	f := newCheckoutFixture(t)
	writeFile(t, filepath.Join(f.root, "build.log"), "artifact\n")
	if _, err := f.engine.run(context.Background(), f.second); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(f.root, "build.log")) != "artifact\n" {
		t.Fatal("ignored file changed")
	}
}

func TestCheckoutNeverOverwritesIgnoredFiles(t *testing.T) {
	f := newCheckoutFixture(t)
	writeFile(t, filepath.Join(f.upstream, "out.log"), "tracked\n")
	fixtureGit(t, f.upstream, "add", "--force", "out.log")
	fixtureGit(t, f.upstream, "commit", "--quiet", "-m", "third")
	third := fixtureGit(t, f.upstream, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(f.root, "out.log"), "local artifact\n")
	if _, err := f.engine.run(context.Background(), third); err == nil {
		t.Fatal("checkout overwrote an ignored file")
	}
	if readFile(t, filepath.Join(f.root, "out.log")) != "local artifact\n" || f.head(t) != "ref: refs/heads/main\n" {
		t.Fatal("ignored file or HEAD changed")
	}
}

func TestCheckoutRefusesUnavailableCommit(t *testing.T) {
	f := newCheckoutFixture(t)
	_, err := f.engine.run(context.Background(), strings.Repeat("1", 40))
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing commit: %v", err)
	}
	if f.head(t) != "ref: refs/heads/main\n" {
		t.Fatal("HEAD changed")
	}
}

// plantObject replaces a loose object with other content under the same name.
func plantObject(t *testing.T, root, oid, kind string, content []byte) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatalf("fixture object is not loose: %v", err)
	}
	writeLooseObject(t, root, oid, kind, content)
}

// writeLooseObject stores content under oid without checking that they match.
func writeLooseObject(t *testing.T, root, oid, kind string, content []byte) {
	t.Helper()
	path := filepath.Join(root, ".git", "objects", oid[:2], oid[2:])
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write([]byte(kind + " " + itoa(len(content)) + "\x00"))
	w.Write(content)
	w.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0600)
	if err := os.WriteFile(path, buf.Bytes(), 0400); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestCheckoutRehashesSnapshotObjects(t *testing.T) {
	t.Run("blob", func(t *testing.T) {
		f := newCheckoutFixture(t)
		fixtureGit(t, f.root, "fetch", "--quiet", "--", f.upstream, f.second)
		plantObject(t, f.root, fixtureGit(t, f.root, "rev-parse", f.second+":tracked"), "blob", []byte("forged\n"))
		if _, err := f.engine.run(context.Background(), f.second); err == nil || !strings.Contains(err.Error(), "hash") {
			t.Fatalf("forged blob accepted: %v", err)
		}
	})
	t.Run("commit", func(t *testing.T) {
		f := newCheckoutFixture(t)
		fixtureGit(t, f.root, "fetch", "--quiet", "--", f.upstream, f.second)
		forged := "tree " + fixtureGit(t, f.root, "rev-parse", f.first+"^{tree}") + "\nauthor A <a@a> 0 +0000\ncommitter A <a@a> 0 +0000\n\nforged\n"
		plantObject(t, f.root, f.second, "commit", []byte(forged))
		// Git сам перепроверяет хэш commit и tree при разборе; blob — нет.
		if _, err := f.engine.run(context.Background(), f.second); err == nil || !strings.Contains(err.Error(), "HEAD and files unchanged") {
			t.Fatalf("forged commit accepted: %v", err)
		}
		if f.head(t) != "ref: refs/heads/main\n" {
			t.Fatal("HEAD moved to a forged commit")
		}
	})
}

func TestCheckoutRefusesUnsafeRepositories(t *testing.T) {
	t.Run("info attributes", func(t *testing.T) {
		f := newCheckoutFixture(t)
		writeFile(t, filepath.Join(f.root, ".git", "info", "attributes"), "* ident\n")
		if _, err := f.engine.run(context.Background(), f.second); err == nil {
			t.Fatal("repository attributes accepted")
		}
	})
	t.Run("config", func(t *testing.T) {
		f := newCheckoutFixture(t)
		fixtureGit(t, f.root, "config", "core.fsmonitor", "/tmp/evil")
		if _, err := f.engine.run(context.Background(), f.second); err == nil {
			t.Fatal("executable configuration accepted")
		}
	})
	t.Run("submodule", func(t *testing.T) {
		f := newCheckoutFixture(t)
		fixtureGit(t, f.upstream, "update-index", "--add", "--cacheinfo", "160000,"+f.first+",sub")
		fixtureGit(t, f.upstream, "commit", "--quiet", "-m", "gitlink")
		third := fixtureGit(t, f.upstream, "rev-parse", "HEAD")
		if _, err := f.engine.run(context.Background(), third); err == nil || !strings.Contains(err.Error(), "submodule") {
			t.Fatalf("gitlink accepted: %v", err)
		}
	})
}

func TestParseCheckoutRequest(t *testing.T) {
	commit := strings.Repeat("ab", 20)
	valid := `{"commit":"` + commit + `","origin":"https://git.example.com/group/project","mode":"gateway"}`
	r, err := parseCheckoutRequest(json.RawMessage(valid))
	if err != nil || r.Commit != commit || r.Mode != "gateway" {
		t.Fatal(r, err)
	}
	e := newCheckoutEngine(r)
	if e.source != GatewayURL || e.protocol != "http" || e.proxied || e.root != fixedWorkspace || e.home != actorHome {
		t.Fatalf("gateway engine: %+v", e)
	}
	r.Mode = "direct"
	if e = newCheckoutEngine(r); e.source != r.Origin || e.protocol != "https" || !e.proxied {
		t.Fatalf("direct engine: %+v", e)
	}
	for _, raw := range []string{
		`null`, `{}`, `[]`,
		`{"commit":"` + commit + `","origin":"https://git.example.com/group/project","mode":"gateway","extra":1}`,
		`{"commit":"` + strings.ToUpper(commit) + `","origin":"https://git.example.com/group/project","mode":"gateway"}`,
		`{"commit":"HEAD","origin":"https://git.example.com/group/project","mode":"gateway"}`,
		`{"commit":"` + commit + `","origin":"https://git.example.com/group/project","mode":"file"}`,
		`{"commit":"` + commit + `","origin":"http://git.example.com/group/project","mode":"direct"}`,
		`{"commit":"` + commit + `","origin":"","mode":"gateway"}`,
		`{"commit":"` + commit + `","origin":null,"mode":"gateway"}`,
	} {
		if _, err := parseCheckoutRequest(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestRunCheckoutRequiresActorIdentity(t *testing.T) {
	if os.Getuid() == 1000 {
		t.Skip("test runs as the actor UID")
	}
	r := RunCheckout(context.Background(), workspaceRequest(CheckoutOperation, `{}`))
	if r.OK || !strings.Contains(r.Text, "UID") {
		t.Fatalf("%+v", r)
	}
}
