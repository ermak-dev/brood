//go:build linux

package sessionctl

// FEATURE: Наблюдение committed result без изменения workspace (#290).
// SPEC: Реальный Git, verified base/head, полный набор old/new paths rename/copy,
// staged/unstaged/untracked/unsafe metadata — отказ, не пустой diff.

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestObserveResultIncludesChangedDeletedRenamedAndCopiedPaths(t *testing.T) {
	f := newCheckoutFixtureWith(t, map[string]string{"go/store/rename": "rename source content long enough\n", "go/store/copy": "copy source content long enough\n", "go/store/deleted": "deleted\n"})
	fixtureGit(t, f.root, "mv", "go/store/rename", "renamed")
	writeFile(t, filepath.Join(f.root, "copied"), readFile(t, filepath.Join(f.root, "go/store/copy")))
	mustRemove := func(path string) {
		t.Helper()
		if err := os.Remove(filepath.Join(f.root, path)); err != nil {
			t.Fatal(err)
		}
	}
	mustRemove("go/store/deleted")
	writeFile(t, filepath.Join(f.root, "tracked"), "changed\n")
	fixtureGit(t, f.root, "add", "--all")
	fixtureGit(t, f.root, "commit", "--quiet", "-m", "result")
	head := fixtureGit(t, f.root, "rev-parse", "HEAD")
	beforeHead, beforeIndex, beforeConfig := f.head(t), readFile(t, filepath.Join(f.root, ".git/index")), readFile(t, filepath.Join(f.root, ".git/config"))
	result, err := f.engine.observeResult(context.Background(), f.first)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"copied", "go/store/copy", "go/store/deleted", "go/store/rename", "renamed", "tracked"}
	if result.Base != f.first || result.Head != head || !reflect.DeepEqual(result.Paths, want) {
		t.Fatalf("incomplete result: %+v", result)
	}
	if f.head(t) != beforeHead || readFile(t, filepath.Join(f.root, ".git/index")) != beforeIndex || readFile(t, filepath.Join(f.root, ".git/config")) != beforeConfig {
		t.Fatal("observation rewrote workspace metadata")
	}
	if readFile(t, filepath.Join(f.root, "tracked")) != "changed\n" {
		t.Fatal("observation rewrote result files")
	}
}

func TestObserveResultRejectsLocalWorkWithoutChangingIt(t *testing.T) {
	for _, kind := range []string{"staged", "unstaged", "untracked", "skip-worktree"} {
		t.Run(kind, func(t *testing.T) {
			f := newCheckoutFixture(t)
			if kind == "untracked" {
				writeFile(t, filepath.Join(f.root, "untracked"), "preserved\n")
			} else {
				if kind == "skip-worktree" {
					fixtureGit(t, f.root, "update-index", "--skip-worktree", "tracked")
				}
				writeFile(t, filepath.Join(f.root, "tracked"), "preserved\n")
				if kind == "staged" {
					fixtureGit(t, f.root, "add", "tracked")
				}
			}
			head, index := f.head(t), readFile(t, filepath.Join(f.root, ".git/index"))
			if result, err := f.engine.observeResult(context.Background(), f.first); err == nil || result.Head != "" {
				t.Fatalf("dirty work reported complete: %+v %v", result, err)
			}
			if f.head(t) != head || readFile(t, filepath.Join(f.root, ".git/index")) != index {
				t.Fatal("observation changed dirty state")
			}
			path := "tracked"
			if kind == "untracked" {
				path = "untracked"
			}
			if readFile(t, filepath.Join(f.root, path)) != "preserved\n" {
				t.Fatal("local work was discarded")
			}
		})
	}
}

func TestObserveResultRefusesUnsafeMetadataAndMissingBase(t *testing.T) {
	for _, kind := range []string{"unsafe-config", "attributes", "missing-base", "corrupt-object"} {
		t.Run(kind, func(t *testing.T) {
			f := newCheckoutFixture(t)
			base := f.first
			switch kind {
			case "unsafe-config":
				fixtureGit(t, f.root, "config", "core.fsmonitor", "/does-not-run")
			case "attributes":
				writeFile(t, filepath.Join(f.root, ".git/info/attributes"), "* text\n")
			case "missing-base":
				base = strings.Repeat("a", 40)
			case "corrupt-object":
				object := filepath.Join(f.root, ".git/objects", f.first[:2], f.first[2:])
				if err := os.Remove(object); err != nil {
					t.Fatal(err)
				}
				writeFile(t, object, "corrupt")
			}
			if result, err := f.engine.observeResult(context.Background(), base); err == nil || result.Head != "" {
				t.Fatalf("unsafe result accepted: %+v %v", result, err)
			}
		})
	}
}

func TestResultDiffParserRejectsTruncationAndUnsafePaths(t *testing.T) {
	for _, raw := range []string{"M\x00path", "M\x00", "M\x00../escape\x00", "M\x00/absolute\x00", "R100\x00old\x00", "C101\x00old\x00new\x00", "X\x00path\x00", "M\x00bad\xff\x00", "M\x00line\nname\x00", "M\x00" + strings.Repeat("x", 1025) + "\x00", strings.Repeat("A\x00path\x00", 2049)} {
		if _, err := resultPaths(bufio.NewReader(strings.NewReader(raw))); err == nil {
			t.Fatal("malformed or over-limit diff accepted")
		}
	}
	got, err := resultPaths(bufio.NewReader(strings.NewReader("R100\x00old\x00new\x00M\x00new\x00")))
	if err != nil || !reflect.DeepEqual(got, []string{"new", "old"}) {
		t.Fatalf("rename and dedup: %v %v", got, err)
	}
}
