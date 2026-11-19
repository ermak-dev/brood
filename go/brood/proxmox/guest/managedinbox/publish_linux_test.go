//go:build linux

package managedinbox

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// FEATURE: Publish the exact opt-in marketplace beneath a trusted guest root.
// SPEC: Publish all bytes at once; accept an existing exact tree without replacing
// it. Refuse unsafe ancestors, links, or changed entries without returning settings.
// IMPLEMENTATION: The fixture hands the production publisher a pinned test root.
func TestPublishRequiresOptIn(t *testing.T) {
	got, err := Publish(false)
	if err != nil || !reflect.DeepEqual(got, Publication{}) {
		t.Fatalf("disabled Publish = %#v, %v; want no publication", got, err)
	}
}

func TestPublishFromExactTree(t *testing.T) {
	root, parent, publish := publicationFixture(t)
	final := filepath.Join(parent, "supervisor-inbox")
	bundle, _ := Package(true)
	want := Publication{Root: bundle.Root, ManagedSettings: bundle.ManagedSettings}
	got, err := publish()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("publish = %#v, %v; want the fixed root and exact settings", got, err)
	}

	files := make(map[string]File)
	dirs := map[string]bool{".": true}
	for _, file := range bundle.Files {
		files[file.Path] = file
		for dir := filepath.Dir(file.Path); dir != "."; dir = filepath.Dir(dir) {
			dirs[dir] = true
		}
	}
	original := make(map[string]os.FileInfo)
	err = filepath.Walk(final, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(final, path)
		if err != nil {
			return err
		}
		original[path] = info
		if info.IsDir() {
			if !dirs[rel] || info.Mode() != os.ModeDir|0o755 {
				t.Fatalf("unexpected directory %s (%s)", rel, info.Mode())
			}
			delete(dirs, rel)
			assertPublicationOwner(t, path, false)
			return nil
		}
		file, ok := files[rel]
		if !ok || info.Mode() != file.Mode {
			t.Fatalf("unexpected file %s (%s)", rel, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, file.Data) {
			t.Fatalf("%s did not contain exact embedded bytes: %v", rel, err)
		}
		delete(files, rel)
		assertPublicationOwner(t, path, true)
		return nil
	})
	if err != nil || len(dirs) != 0 || len(files) != 0 {
		t.Fatalf("incomplete tree: dirs=%v files=%v err=%v", dirs, files, err)
	}
	assertPublicationOwner(t, parent, false)
	got.ManagedSettings[0] ^= 0xff
	if got, err = publish(); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("exact tree republication = %#v, %v", got, err)
	}
	for path, before := range original {
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("republication replaced %s: %v", path, err)
		}
	}
	assertPublicationChildren(t, parent)
	if _, err := os.Lstat(filepath.Join(root, "etc")); !os.IsNotExist(err) {
		t.Fatalf("publish touched settings on disk: %v", err)
	}
}

func TestPublishFromRefusesUnsafeAncestors(t *testing.T) {
	t.Run("wrong owner at the anchor", func(t *testing.T) {
		root, parent, _ := publicationFixture(t)
		fd, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer fd.Close()
		got, err := publishFrom(int(fd.Fd()), uint32(os.Geteuid())+1, uint32(os.Getegid()))
		if err == nil || !reflect.DeepEqual(got, Publication{}) {
			t.Fatalf("accepted wrong owner: %#v, %v", got, err)
		}
		if _, err := os.Lstat(parent); !os.IsNotExist(err) {
			t.Fatalf("modified untrusted tree: %v", err)
		}
	})
	for _, component := range []string{".", "opt", "opt/brood", "opt/brood/managed-marketplaces"} {
		t.Run("writable "+component, func(t *testing.T) {
			root, parent, publish := publicationFixture(t)
			makePublicationDir(t, parent)
			path := filepath.Join(root, component)
			if err := os.Chmod(path, 0o775); err != nil {
				t.Fatal(err)
			}
			assertPublishRefusal(t, publish)
			info, err := os.Lstat(path)
			if err != nil || info.Mode() != os.ModeDir|0o775 {
				t.Fatalf("publisher altered untrusted ancestor: %v, %v", info, err)
			}
			assertPublicationEmpty(t, parent)
		})
	}
	for _, component := range []string{"opt", "opt/brood", "opt/brood/managed-marketplaces"} {
		t.Run("symlink "+component, func(t *testing.T) {
			root, _, publish := publicationFixture(t)
			path := filepath.Join(root, component)
			target := filepath.Join(t.TempDir(), "foreign")
			makePublicationDir(t, target)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			assertPublishRefusal(t, publish)
			if got, err := os.Readlink(path); err != nil || got != target {
				t.Fatalf("changed untrusted symlink: %q, %v", got, err)
			}
			assertPublicationEmpty(t, target)
		})
	}
}

func TestPublishFromRefusesExistingChanges(t *testing.T) {
	const hook = "plugins/supervisor-inbox/hooks/register.js"
	const hooks = "plugins/supervisor-inbox/hooks"
	for _, tc := range []struct {
		name  string
		path  string
		alter func(*testing.T, string) error
	}{
		{"missing file", hook, func(_ *testing.T, p string) error { return os.Remove(p) }},
		{"same size different bytes", hook, func(_ *testing.T, p string) error {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			data[0] ^= 0xff
			return os.WriteFile(p, data, 0o644)
		}},
		{"unexpected file", hooks + "/extra.js", func(_ *testing.T, p string) error {
			return os.WriteFile(p, []byte("unexpected"), 0o644)
		}},
		{"file symlink to exact bytes", hook, func(t *testing.T, p string) error {
			target := filepath.Join(t.TempDir(), "foreign.js")
			if err := os.Rename(p, target); err != nil {
				return err
			}
			return os.Symlink(target, p)
		}},
		{"directory symlink to exact tree", hooks, func(t *testing.T, p string) error {
			target := filepath.Join(t.TempDir(), "foreign")
			if err := os.Rename(p, target); err != nil {
				return err
			}
			return os.Symlink(target, p)
		}},
		{"root symlink to exact tree", ".", func(t *testing.T, p string) error {
			target := filepath.Join(t.TempDir(), "foreign")
			if err := os.Rename(p, target); err != nil {
				return err
			}
			return os.Symlink(target, p)
		}},
		{"hard-linked exact bytes", hook, func(t *testing.T, p string) error {
			return os.Link(p, filepath.Join(t.TempDir(), "foreign.js"))
		}},
		{"writable file", hook, func(_ *testing.T, p string) error { return os.Chmod(p, 0o664) }},
		{"writable directory", hooks, func(_ *testing.T, p string) error { return os.Chmod(p, 0o777) }},
		{"writable root", ".", func(_ *testing.T, p string) error { return os.Chmod(p, 0o775) }},
		{"foreign-owned file", hook, func(t *testing.T, p string) error {
			if os.Geteuid() != 0 {
				t.Skip("changing fixture ownership requires root")
			}
			return os.Chown(p, 1000, -1)
		}},
		{"fifo", hook, func(_ *testing.T, p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return unix.Mkfifo(p, 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, parent, publish := publicationFixture(t)
			if _, err := publish(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "supervisor-inbox", tc.path)
			if err := tc.alter(t, path); err != nil {
				t.Fatal(err)
			}
			before, statErr := os.Lstat(path)
			assertPublishRefusal(t, publish)
			after, err := os.Lstat(path)
			if os.IsNotExist(statErr) {
				if !os.IsNotExist(err) {
					t.Fatalf("recreated missing entry: %v", err)
				}
			} else if statErr != nil || err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("changed existing entry: before %v (%v), after %v (%v)", before, statErr, after, err)
			}
			assertPublicationChildren(t, parent)
		})
	}
}

func TestPublishFromConcurrent(t *testing.T) {
	_, parent, publish := publicationFixture(t)
	makePublicationDir(t, parent)
	const calls = 8
	start := make(chan struct{})
	done := make(chan error, calls)
	for range calls {
		go func() {
			<-start
			_, err := publish()
			done <- err
		}()
	}
	close(start)
	for range calls {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	assertPublicationChildren(t, parent)
}

const (
	stateTestMarket = "/opt/brood/managed-marketplaces/supervisor-inbox"
	stateTestRoot   = "/opt/brood/managed-plugin-state/supervisor-inbox"
	stateTestCache  = stateTestRoot + "/cache/brood-managed-inbox/supervisor-inbox/0.1.0"
	stateTestKnown  = stateTestRoot + "/known_marketplaces.json"
	stateTestIndex  = stateTestRoot + "/installed_plugins.json"
	stateTestHook   = "/hooks/register.js"

	stateTestMarketRecord = `{"source":{"source":"directory","path":"` + stateTestMarket + `"},"installLocation":"` + stateTestMarket + `"}`
	stateTestPluginRecord = `{"scope":"user","version":"0.1.0","installPath":"` + stateTestCache + `","installedAt":"2026-11-19T00:00:00Z","lastUpdated":"2026-11-19T00:00:00Z"}`
	stateTestKnownJSON    = `{"brood-managed-inbox":` + stateTestMarketRecord + `}`
	stateTestIndexJSON    = `{"version":2,"plugins":{"supervisor-inbox@brood-managed-inbox":[` + stateTestPluginRecord + `]}}`
)

func TestVerifyPluginStateFrom(t *testing.T) {
	t.Run("exact seed", func(t *testing.T) {
		root, verify := pluginStateFixture(t)
		if err := verify(); err != nil {
			t.Fatalf("refused exact seed: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "etc")); !os.IsNotExist(err) {
			t.Fatalf("verification touched guest configuration: %v", err)
		}
	})

	for _, tc := range []struct{ name, path, registry, from, to string }{
		{"wrong marketplace source path", stateTestKnown, stateTestKnownJSON, `"path":"` + stateTestMarket + `"`, `"path":"` + stateTestMarket + `/plugins/supervisor-inbox"`},
		{"wrong marketplace install location", stateTestKnown, stateTestKnownJSON, `"installLocation":"` + stateTestMarket + `"`, `"installLocation":"` + stateTestMarket + `/plugins/supervisor-inbox"`},
		{"non-directory marketplace source", stateTestKnown, stateTestKnownJSON, `"source":"directory"`, `"source":"file"`},
		{"non-user install", stateTestIndex, stateTestIndexJSON, `"scope":"user"`, `"scope":"project"`},
		{"wrong plugin version", stateTestIndex, stateTestIndexJSON, `"version":"0.1.0"`, `"version":"0.1.1"`},
		{"install points at marketplace copy", stateTestIndex, stateTestIndexJSON, stateTestCache, stateTestMarket + "/plugins/supervisor-inbox"},
		{"duplicate install record", stateTestIndex, stateTestIndexJSON, stateTestPluginRecord, stateTestPluginRecord + "," + stateTestPluginRecord},
		{"extra installed plugin", stateTestIndex, stateTestIndexJSON, `"plugins":{`, `"plugins":{"other@brood-managed-inbox":[` + stateTestPluginRecord + `],`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, verify := pluginStateFixture(t)
			writePluginStateFile(t, root, tc.path, []byte(strings.Replace(tc.registry, tc.from, tc.to, 1)))
			if err := verify(); err == nil {
				t.Fatal("accepted invalid registry")
			}
		})
	}

	for _, tc := range []struct {
		name, path string
		alter      func(t *testing.T, root, path string) error
	}{
		{"changed cache bytes", stateTestCache + stateTestHook, func(_ *testing.T, _, path string) error {
			data := bytes.Clone(hookModule)
			data[0] ^= 0xff
			return os.WriteFile(path, data, 0o644)
		}},
		{"extra cache entry", stateTestCache + "/hooks/extra.js", func(_ *testing.T, _, path string) error {
			if err := os.WriteFile(path, []byte("extra"), 0o644); err != nil {
				return err
			}
			return os.Chmod(path, 0o644)
		}},
		{"cache symlink to exact bytes", stateTestCache + stateTestHook, func(_ *testing.T, root, path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(root, stateTestMarket, pluginPath, stateTestHook), path)
		}},
		{"hard-linked cache file", stateTestCache + stateTestHook, func(_ *testing.T, root, path string) error {
			return os.Link(path, filepath.Join(root, "other-link"))
		}},
		{"user-writable cache directory", stateTestCache, func(_ *testing.T, _, path string) error {
			return os.Chmod(path, 0o777)
		}},
		{"user-writable registry", stateTestKnown, func(_ *testing.T, _, path string) error {
			return os.Chmod(path, 0o666)
		}},
		{"foreign-owned cache file", stateTestCache + stateTestHook, func(t *testing.T, _, path string) error {
			if os.Geteuid() != 0 {
				t.Skip("changing fixture ownership requires root")
			}
			return os.Chown(path, 1000, -1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, verify := pluginStateFixture(t)
			if err := tc.alter(t, root, filepath.Join(root, tc.path)); err != nil {
				t.Fatal(err)
			}
			if err := verify(); err == nil {
				t.Fatal("accepted invalid plugin state")
			}
		})
	}

	t.Run("missing cache is not provisioned", func(t *testing.T) {
		root, verify := pluginStateFixture(t)
		cache := filepath.Join(root, stateTestRoot, "cache")
		if err := os.RemoveAll(cache); err != nil {
			t.Fatal(err)
		}
		if err := verify(); err == nil {
			t.Error("accepted missing cache")
		}
		if _, err := os.Lstat(cache); !os.IsNotExist(err) {
			t.Fatalf("verification did not leave the cache absent: %v", err)
		}
	})
}

func pluginStateFixture(t *testing.T) (root string, verify func() error) {
	t.Helper()
	root = t.TempDir()
	makePublicationDir(t, root)
	bundle, _ := Package(true)
	for _, file := range bundle.Files {
		writePluginStateFile(t, root, stateTestMarket+"/"+file.Path, file.Data)
		if rel, ok := strings.CutPrefix(file.Path, pluginPath); ok {
			writePluginStateFile(t, root, stateTestCache+"/"+rel, file.Data)
		}
	}
	writePluginStateFile(t, root, stateTestKnown, []byte(stateTestKnownJSON))
	writePluginStateFile(t, root, stateTestIndex, []byte(stateTestIndexJSON))
	fd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	return root, func() error {
		return verifyPluginStateFrom(int(fd.Fd()), uint32(os.Geteuid()), uint32(os.Getegid()))
	}
}

func writePluginStateFile(t *testing.T, root, guestPath string, data []byte) {
	t.Helper()
	dir := root
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(guestPath), "/"), "/") {
		dir = filepath.Join(dir, part)
		makePublicationDir(t, dir)
	}
	path := filepath.Join(dir, filepath.Base(guestPath))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

func publicationFixture(t *testing.T) (root, parent string, publish func() (Publication, error)) {
	t.Helper()
	root = t.TempDir()
	makePublicationDir(t, root)
	makePublicationDir(t, filepath.Join(root, "opt"))
	makePublicationDir(t, filepath.Join(root, "opt/brood"))
	fd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	return root, filepath.Join(root, "opt/brood/managed-marketplaces"), func() (Publication, error) {
		return publishFrom(int(fd.Fd()), uint32(os.Geteuid()), uint32(os.Getegid()))
	}
}

func makePublicationDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertPublishRefusal(t *testing.T, publish func() (Publication, error)) {
	t.Helper()
	got, err := publish()
	if err == nil || !strings.Contains(err.Error(), "managed inbox") || !reflect.DeepEqual(got, Publication{}) {
		t.Fatalf("expected managed inbox refusal without settings, got %#v, %v", got, err)
	}
}

func assertPublicationOwner(t *testing.T, path string, singleLink bool) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid != uint32(os.Geteuid()) || st.Gid != uint32(os.Getegid()) || (singleLink && st.Nlink != 1) {
		t.Fatalf("unexpected ownership at %s: uid=%d gid=%d links=%d", path, st.Uid, st.Gid, st.Nlink)
	}
}

func assertPublicationChildren(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "supervisor-inbox" {
		t.Fatalf("unexpected publication directory contents: %v, %v", entries, err)
	}
}

func assertPublicationEmpty(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected mutation at %s: %v, %v", parent, entries, err)
	}
}
