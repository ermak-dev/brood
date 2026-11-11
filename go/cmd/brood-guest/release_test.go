//go:build linux

package main

// FEATURE: Update guest binaries on every CT start without reset (#184).
// SPEC: The bootstrap installs the server's artifact set into the
// content-addressed directory releases/<id> under the artifact root, verifies
// every file against the manifest before the release becomes visible,
// verifies an existing release again before reusing it without download and
// switches the bin symlink to it atomically. The previous release stays for
// rollback; older releases and unfinished staging are collected. A plain bin
// directory of the earlier bootstrap is moved aside, never deleted. Root
// operations do not follow links and refuse a root writable by others;
// nothing outside the artifact root changes.
// IMPLEMENTATION: real installRelease under temporary roots with an in-memory
// download; crash states are built on disk the way an interrupted start
// leaves them.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"golang.org/x/sys/unix"

	"brood.dev/go/brood/proxmox/guest"
)

var releaseNames = []string{"bro", "brood-guest", "claude"}

// releaseSet: содержимое трёх артефактов одного набора.
type releaseSet map[string]string

func newReleaseSet(tag string) releaseSet {
	return releaseSet{"bro": "\x7fELFbro-" + tag, "brood-guest": "\x7fELFguest-" + tag, "claude": "\x7fELFclaude-" + tag}
}

func (s releaseSet) manifest() guest.ArtifactManifest {
	var m guest.ArtifactManifest
	for _, name := range releaseNames {
		m.Artifacts = append(m.Artifacts, guest.Artifact{Name: name, Size: int64(len(s[name])), SHA256: sha256Hex(s[name])})
	}
	return m
}

func (s releaseSet) id(t *testing.T) string {
	t.Helper()
	id, err := s.manifest().ReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fakeServer: доставка набора без сети: отдаёт тела (или подмену), считает
// загрузки и умеет оборвать поток посреди артефакта.
type fakeServer struct {
	set       releaseSet
	serve     map[string]string
	broken    string
	forbid    *testing.T
	downloads []string
}

func (f *fakeServer) download(ctx context.Context, artifact guest.Artifact, install guest.ArtifactInstaller) error {
	if f.forbid != nil {
		f.forbid.Errorf("verified release downloaded %s again", artifact.Name)
	}
	f.downloads = append(f.downloads, artifact.Name)
	body, ok := f.serve[artifact.Name]
	if !ok {
		body = f.set[artifact.Name]
	}
	var stream io.Reader = strings.NewReader(body)
	if artifact.Name == f.broken {
		stream = io.MultiReader(strings.NewReader(body[:len(body)/2]), iotest.ErrReader(errors.New("connection reset")))
	}
	return install(ctx, artifact.Name, stream, artifact.Size, artifact.SHA256)
}

func newRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "brood")
}

func start(root string, server *fakeServer) (string, error) {
	return installRelease(context.Background(), root, server.set.manifest(), server.download, func(error) {})
}

func mustStart(t *testing.T, root string, server *fakeServer) string {
	t.Helper()
	executable, err := start(root, server)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if want := filepath.Join(root, "releases", server.set.id(t), "brood-guest"); executable != want {
		t.Fatalf("runtime executable %q, want %q", executable, want)
	}
	return executable
}

func linkTarget(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func releaseEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func sortedIDs(t *testing.T, sets ...releaseSet) []string {
	t.Helper()
	ids := []string{}
	for _, set := range sets {
		ids = append(ids, set.id(t))
	}
	sort.Strings(ids)
	return ids
}

// assertActive: bin является symlink на releases/<id> набора, фиксированные пути
// /opt/brood/bin/* отдают его содержимое, файлы и каталоги закрыты на запись.
func assertActive(t *testing.T, root string, set releaseSet) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(root, "bin"))
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("bin is not a symlink: %v %v", info, err)
	}
	if target := linkTarget(t, filepath.Join(root, "bin")); target != "releases/"+set.id(t) {
		t.Fatalf("bin points to %q, want releases/%s", target, set.id(t))
	}
	for _, dir := range []string{root, filepath.Join(root, "releases"), filepath.Join(root, "releases", set.id(t))} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
			t.Fatalf("%s: %v %v", dir, info, err)
		}
	}
	for _, name := range releaseNames {
		data, err := os.ReadFile(filepath.Join(root, "bin", name))
		if err != nil || string(data) != set[name] {
			t.Fatalf("bin/%s: %q %v", name, data, err)
		}
		info, err := os.Lstat(filepath.Join(root, "releases", set.id(t), name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
			t.Fatalf("release file %s: %v %v", name, info, err)
		}
	}
}

func writeSet(t *testing.T, dir string, set releaseSet) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range set {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func readSet(t *testing.T, dir string) releaseSet {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	set := releaseSet{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		set[entry.Name()] = string(data)
	}
	return set
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

func TestReleaseFreshInstall(t *testing.T) {
	root := newRoot(t)
	a := newReleaseSet("a")
	server := &fakeServer{set: a}
	mustStart(t, root, server)
	if !slices.Equal(server.downloads, releaseNames) {
		t.Fatalf("downloads %v", server.downloads)
	}
	assertActive(t, root, a)
	if got := releaseEntries(t, root); !slices.Equal(got, []string{a.id(t)}) {
		t.Fatalf("releases %v", got)
	}
	if previous := linkTarget(t, filepath.Join(root, "previous")); previous != "" {
		t.Fatalf("fresh install has a previous release %q", previous)
	}
	for _, dir := range []string{root, filepath.Join(root, "releases"), filepath.Join(root, "releases", a.id(t))} {
		if info, _ := os.Lstat(dir); info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode %v, want 0755 for the actor to reach bro and claude", dir, info.Mode())
		}
	}
}

func TestReleaseReuseWithoutDownload(t *testing.T) {
	root := newRoot(t)
	a := newReleaseSet("a")
	mustStart(t, root, &fakeServer{set: a})
	before := inode(t, filepath.Join(root, "releases", a.id(t), "claude"))
	mustStart(t, root, &fakeServer{set: a, forbid: t})
	assertActive(t, root, a)
	if after := inode(t, filepath.Join(root, "releases", a.id(t), "claude")); after != before {
		t.Fatal("verified release was reinstalled")
	}
}

// TestReleaseDamagedNeverUsed: повреждённый или неполный releases/<id>
// (совпадающий по имени с манифестом) не используется: набор качается
// заново, а цели ссылок вне корня не трогаются.
func TestReleaseDamagedNeverUsed(t *testing.T) {
	a := newReleaseSet("a")
	damage := map[string]func(t *testing.T, release, outside string){
		"flipped byte": func(t *testing.T, release, _ string) {
			body := []byte(a["claude"])
			body[len(body)-1] ^= 1
			writeFile(t, filepath.Join(release, "claude"), string(body))
		},
		"truncated file": func(t *testing.T, release, _ string) {
			if err := os.Truncate(filepath.Join(release, "bro"), 4); err != nil {
				t.Fatal(err)
			}
		},
		"missing file": func(t *testing.T, release, _ string) { remove(t, filepath.Join(release, "brood-guest")) },
		"extra file":   func(t *testing.T, release, _ string) { writeFile(t, filepath.Join(release, "sh"), "\x7fELFsh") },
		"subdirectory": func(t *testing.T, release, _ string) { mkdir(t, filepath.Join(release, "lib")) },
		"file is a link": func(t *testing.T, release, outside string) {
			writeFile(t, filepath.Join(outside, "claude"), a["claude"])
			remove(t, filepath.Join(release, "claude"))
			symlink(t, filepath.Join(outside, "claude"), filepath.Join(release, "claude"))
		},
		"release is a link": func(t *testing.T, release, outside string) {
			if err := os.Rename(release, filepath.Join(outside, "copy")); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join(outside, "copy"), release)
		},
		"file writable by others": func(t *testing.T, release, _ string) { chmod(t, filepath.Join(release, "claude"), 0o777) },
		"file not executable":     func(t *testing.T, release, _ string) { chmod(t, filepath.Join(release, "bro"), 0o644) },
		"release writable":        func(t *testing.T, release, _ string) { chmod(t, release, 0o777) },
	}
	for name, apply := range damage {
		t.Run(name, func(t *testing.T) {
			root, outside := newRoot(t), t.TempDir()
			mustStart(t, root, &fakeServer{set: a})
			apply(t, filepath.Join(root, "releases", a.id(t)), outside)
			before := snapshotTree(t, outside, "")
			// Без сервера повреждённый набор не загружается (fail closed).
			if executable, err := start(root, &fakeServer{set: a, broken: "bro"}); err == nil || executable != "" {
				t.Fatalf("damaged release used: %q %v", executable, err)
			}
			server := &fakeServer{set: a}
			mustStart(t, root, server)
			if !slices.Equal(server.downloads, releaseNames) {
				t.Fatalf("damaged release not downloaded again: %v", server.downloads)
			}
			assertActive(t, root, a)
			if got := releaseEntries(t, root); !slices.Equal(got, []string{a.id(t)}) {
				t.Fatalf("releases %v", got)
			}
			if after := snapshotTree(t, outside, ""); !reflect.DeepEqual(before, after) {
				t.Fatalf("link target outside the root changed:\n%v\n%v", before, after)
			}
		})
	}
}

// TestReleaseInterruptedInstall: оборванная загрузка оставляет прежний
// набор активным, а следующий старт доводит установку до конца из любого
// состояния, которое оставляет крах между шагами.
func TestReleaseInterruptedInstall(t *testing.T) {
	a, b := newReleaseSet("a"), newReleaseSet("b")
	t.Run("download breaks", func(t *testing.T) {
		root := newRoot(t)
		mustStart(t, root, &fakeServer{set: a})
		for _, name := range releaseNames {
			if executable, err := start(root, &fakeServer{set: b, broken: name}); err == nil || executable != "" {
				t.Fatalf("broken %s accepted: %q %v", name, executable, err)
			}
			assertActive(t, root, a)
			if got := releaseEntries(t, root); !slices.Equal(got, []string{a.id(t)}) {
				t.Fatalf("broken %s left %v", name, got)
			}
		}
		mustStart(t, root, &fakeServer{set: b})
		assertActive(t, root, b)
		if previous := linkTarget(t, filepath.Join(root, "previous")); previous != "releases/"+a.id(t) {
			t.Fatalf("previous %q", previous)
		}
	})
	// published: каталог проверенного набора b, собранный настоящей установкой.
	published := func(t *testing.T) string {
		scratch := newRoot(t)
		mustStart(t, scratch, &fakeServer{set: b})
		return filepath.Join(scratch, "releases", b.id(t))
	}
	crashes := map[string]struct {
		leave    func(t *testing.T, root string)
		download bool
	}{
		// Крах оставляет staging ровно под именем .stage-<id>.
		"during download": {func(t *testing.T, root string) {
			stage := filepath.Join(root, "releases", ".stage-"+b.id(t))
			writeSet(t, stage, releaseSet{"brood-guest": b["brood-guest"], ".bro.part": b["bro"][:3]})
		}, true},
		"before publish": {func(t *testing.T, root string) {
			if err := os.Rename(published(t), filepath.Join(root, "releases", ".stage-"+b.id(t))); err != nil {
				t.Fatal(err)
			}
		}, true},
		"after publish": {func(t *testing.T, root string) {
			if err := os.Rename(published(t), filepath.Join(root, "releases", b.id(t))); err != nil {
				t.Fatal(err)
			}
		}, false},
		"switch link prepared": {func(t *testing.T, root string) {
			symlink(t, "releases/"+b.id(t), filepath.Join(root, ".bin.next"))
			symlink(t, "releases/"+a.id(t), filepath.Join(root, ".previous.next"))
		}, true},
		"previous moved before bin": {func(t *testing.T, root string) {
			if err := os.Rename(published(t), filepath.Join(root, "releases", b.id(t))); err != nil {
				t.Fatal(err)
			}
			symlink(t, "releases/"+a.id(t), filepath.Join(root, "previous"))
		}, false},
	}
	for name, crash := range crashes {
		t.Run(name, func(t *testing.T) {
			root := newRoot(t)
			mustStart(t, root, &fakeServer{set: a})
			crash.leave(t, root)
			server := &fakeServer{set: b}
			if !crash.download {
				server.forbid = t
			}
			mustStart(t, root, server)
			assertActive(t, root, b)
			if previous := linkTarget(t, filepath.Join(root, "previous")); previous != "releases/"+a.id(t) {
				t.Fatalf("previous %q", previous)
			}
			if got := releaseEntries(t, root); !slices.Equal(got, sortedIDs(t, a, b)) {
				t.Fatalf("releases %v", got)
			}
			for _, stale := range []string{".bin.next", ".previous.next"} {
				if _, err := os.Lstat(filepath.Join(root, stale)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("%s left behind: %v", stale, err)
				}
			}
		})
	}
}

// TestReleaseSwitchIsAtomic: пока bootstrap переключает наборы, bin всегда
// существует и указывает на полный проверенный набор. inotify доказывает
// механизм: имя bin только заменяется rename-ом и не удаляется, а
// releases/<id> появляется переименованием готового staging, не создаётся.
func TestReleaseSwitchIsAtomic(t *testing.T) {
	root := newRoot(t)
	a, b, c := newReleaseSet("a"), newReleaseSet("b"), newReleaseSet("c")
	mustStart(t, root, &fakeServer{set: a})
	mustStart(t, root, &fakeServer{set: b})
	rootEvents, releaseEvents := watch(t, root), watch(t, filepath.Join(root, "releases"))
	sets := map[string]releaseSet{"releases/" + a.id(t): a, "releases/" + b.id(t): b}
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		for observed := 0; ; observed++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			target, err := os.Readlink(filepath.Join(root, "bin"))
			set, ok := sets[target]
			if err != nil || !ok {
				done <- fmt.Errorf("observation %d: bin %q %v", observed, target, err)
				return
			}
			for _, name := range releaseNames {
				if data, err := os.ReadFile(filepath.Join(root, target, name)); err != nil || string(data) != set[name] {
					done <- fmt.Errorf("release %s is incomplete: %s %v", target, name, err)
					return
				}
				data, err := os.ReadFile(filepath.Join(root, "bin", name))
				if err != nil || string(data) != a[name] && string(data) != b[name] {
					done <- fmt.Errorf("bin/%s unavailable mid-switch: %q %v", name, data, err)
					return
				}
			}
		}
	}()
	for i := 0; i < 40; i++ {
		set := a
		if i%2 == 1 {
			set = b
		}
		mustStart(t, root, &fakeServer{set: set, forbid: t})
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mustStart(t, root, &fakeServer{set: c})
	switches := 0
	for _, event := range rootEvents() {
		if strings.HasSuffix(event, " bin") || strings.HasSuffix(event, " previous") {
			if !strings.HasPrefix(event, "to ") {
				t.Fatalf("link replaced non-atomically: %q", event)
			}
			switches++
		}
	}
	if switches == 0 {
		t.Fatal("no switch observed")
	}
	published := false
	for _, event := range releaseEvents() {
		switch event {
		case "create " + c.id(t):
			t.Fatalf("release directory created in place: %q", event)
		case "to " + c.id(t):
			published = true
		}
	}
	if !published {
		t.Fatal("release not published by rename")
	}
}

// watch подписывается inotify на имена каталога dir; вызов возвращает
// накопленные события вида "create|delete|from|to имя".
func watch(t *testing.T, dir string) func() []string {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO); err != nil {
		t.Fatal(err)
	}
	kinds := []struct {
		mask uint32
		name string
	}{{unix.IN_CREATE, "create"}, {unix.IN_DELETE, "delete"}, {unix.IN_MOVED_FROM, "from"}, {unix.IN_MOVED_TO, "to"}}
	return func() []string {
		events := []string{}
		buf := make([]byte, 64<<10)
		for {
			n, err := unix.Read(fd, buf)
			if errors.Is(err, unix.EAGAIN) {
				return events
			}
			if err != nil {
				t.Fatal(err)
			}
			for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
				mask := binary.NativeEndian.Uint32(buf[offset+4:])
				length := int(binary.NativeEndian.Uint32(buf[offset+12:]))
				name := strings.TrimRight(string(buf[offset+unix.SizeofInotifyEvent:offset+unix.SizeofInotifyEvent+length]), "\x00")
				for _, kind := range kinds {
					if mask&kind.mask != 0 {
						events = append(events, kind.name+" "+name)
					}
				}
				offset += unix.SizeofInotifyEvent + length
			}
		}
	}
}

func TestReleaseRollback(t *testing.T) {
	root := newRoot(t)
	a, b := newReleaseSet("a"), newReleaseSet("b")
	mustStart(t, root, &fakeServer{set: a})
	mustStart(t, root, &fakeServer{set: b})
	// Сервер вернул прежний набор: он берётся с диска без загрузки.
	mustStart(t, root, &fakeServer{set: a, forbid: t})
	assertActive(t, root, a)
	if previous := linkTarget(t, filepath.Join(root, "previous")); previous != "releases/"+b.id(t) {
		t.Fatalf("previous after rollback %q", previous)
	}
	// Повторный старт того же набора не теряет цель отката.
	mustStart(t, root, &fakeServer{set: a, forbid: t})
	if previous := linkTarget(t, filepath.Join(root, "previous")); previous != "releases/"+b.id(t) {
		t.Fatalf("restart replaced previous: %q", previous)
	}
	mustStart(t, root, &fakeServer{set: b, forbid: t})
	assertActive(t, root, b)
	if got := releaseEntries(t, root); !slices.Equal(got, sortedIDs(t, a, b)) {
		t.Fatalf("releases %v", got)
	}
}

// TestReleaseCollectsOldReleases: после старта на диске не больше двух
// наборов: текущий и прежний; мусор в releases уходит без следования ссылкам.
func TestReleaseCollectsOldReleases(t *testing.T) {
	root := newRoot(t)
	sets := []releaseSet{newReleaseSet("a"), newReleaseSet("b"), newReleaseSet("c"), newReleaseSet("d")}
	for i, set := range sets {
		mustStart(t, root, &fakeServer{set: set})
		if got := releaseEntries(t, root); len(got) > 2 {
			t.Fatalf("start %d keeps %d releases", i, len(got))
		}
		if i > 0 {
			if previous := linkTarget(t, filepath.Join(root, "previous")); previous != "releases/"+sets[i-1].id(t) {
				t.Fatalf("start %d previous %q", i, previous)
			}
		}
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep"), "data")
	releases := filepath.Join(root, "releases")
	writeSet(t, filepath.Join(releases, ".stage-x"), releaseSet{".bro.part": "\x7fE"})
	writeFile(t, filepath.Join(releases, "junk"), "x")
	symlink(t, outside, filepath.Join(releases, "link"))
	writeSet(t, filepath.Join(releases, newReleaseSet("e").id(t)), newReleaseSet("e"))
	mustStart(t, root, &fakeServer{set: sets[3], forbid: t})
	assertActive(t, root, sets[3])
	if got := releaseEntries(t, root); !slices.Equal(got, sortedIDs(t, sets[2], sets[3])) {
		t.Fatalf("releases %v", got)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(data) != "data" {
		t.Fatalf("collection followed a link: %q %v", data, err)
	}
}

// TestReleaseCleanupFailureDoesNotBlockStart: запись, которую уборка удалить
// не может, не останавливает старт ни до загрузки, ни после переключения:
// bin указывает на проверенный набор, а иначе тот же отказ повторялся бы на
// каждом старте и выводил актора из строя навсегда. Отказ уборки виден
// предупреждением, остальное удаляемое уходит. Каталог глубже removeDepth
// не удаляется и под root, в отличие от прав доступа.
func TestReleaseCleanupFailureDoesNotBlockStart(t *testing.T) {
	root := newRoot(t)
	a, b := newReleaseSet("a"), newReleaseSet("b")
	mustStart(t, root, &fakeServer{set: a})
	releases := filepath.Join(root, "releases")
	deep := filepath.Join(releases, "stuck")
	for i := 0; i <= removeDepth+1; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(releases, "junk"), "x")
	var warnings []error
	executable, err := installRelease(context.Background(), root, b.manifest(), (&fakeServer{set: b}).download, func(err error) { warnings = append(warnings, err) })
	if err != nil {
		t.Fatalf("cleanup failure blocked the start: %v", err)
	}
	if want := filepath.Join(releases, b.id(t), "brood-guest"); executable != want {
		t.Fatalf("runtime executable %q, want %q", executable, want)
	}
	assertActive(t, root, b)
	if len(warnings) == 0 || !errors.Is(warnings[0], errReleaseCleanup) {
		t.Fatalf("cleanup failure not reported: %v", warnings)
	}
	if _, err := os.Lstat(filepath.Join(releases, "junk")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("one stuck entry stopped the rest of the cleanup: %v", err)
	}
	// Следующий старт того же набора тоже проходит.
	if _, err := installRelease(context.Background(), root, b.manifest(), (&fakeServer{set: b, forbid: t}).download, func(error) {}); err != nil {
		t.Fatalf("second start refused: %v", err)
	}
}

// TestReleaseMigratesPlainBin: плоский /opt/brood/bin прежнего bootstrap
// отодвигается переименованием с сохранением содержимого.
func TestReleaseMigratesPlainBin(t *testing.T) {
	root := newRoot(t)
	old, a := newReleaseSet("old"), newReleaseSet("a")
	writeSet(t, filepath.Join(root, "bin"), old)
	writeSet(t, filepath.Join(root, "tmp"), releaseSet{"bro.12.leftover": "x"})
	mustStart(t, root, &fakeServer{set: a})
	assertActive(t, root, a)
	if got := readSet(t, filepath.Join(root, "bin.legacy-1")); !reflect.DeepEqual(got, old) {
		t.Fatalf("legacy bin content %v", got)
	}
	if got := readSet(t, filepath.Join(root, "tmp")); !reflect.DeepEqual(got, releaseSet{"bro.12.leftover": "x"}) {
		t.Fatalf("legacy staging changed: %v", got)
	}
	mustStart(t, root, &fakeServer{set: a, forbid: t})
	if _, err := os.Lstat(filepath.Join(root, "bin.legacy-2")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("symlinked bin moved aside again: %v", err)
	}
	// Ещё один не-symlink bin получает следующее имя, прежнее не трогается.
	remove(t, filepath.Join(root, "bin"))
	writeFile(t, filepath.Join(root, "bin"), "plain file")
	mustStart(t, root, &fakeServer{set: a, forbid: t})
	assertActive(t, root, a)
	if data, err := os.ReadFile(filepath.Join(root, "bin.legacy-2")); err != nil || string(data) != "plain file" {
		t.Fatalf("second legacy bin: %q %v", data, err)
	}
	if got := readSet(t, filepath.Join(root, "bin.legacy-1")); !reflect.DeepEqual(got, old) {
		t.Fatalf("first legacy bin changed: %v", got)
	}
}

// TestReleaseWrongDigestNeverVisible: поток с другим sha256, короче или
// длиннее манифеста не становится набором и не трогает активный (#265).
func TestReleaseWrongDigestNeverVisible(t *testing.T) {
	root := newRoot(t)
	a, b := newReleaseSet("a"), newReleaseSet("b")
	forged := func(body string) string { return body[:len(body)-1] + "X" }
	bad := map[string]map[string]string{
		"wrong digest":  {"bro": forged(a["bro"])},
		"short stream":  {"claude": a["claude"][:4]},
		"longer stream": {"brood-guest": a["brood-guest"] + "X"},
	}
	for name, serve := range bad {
		if executable, err := start(root, &fakeServer{set: a, serve: serve}); err == nil || executable != "" {
			t.Fatalf("%s accepted: %q %v", name, executable, err)
		}
		if got := releaseEntries(t, root); len(got) != 0 {
			t.Fatalf("%s left %v", name, got)
		}
		if _, err := os.Lstat(filepath.Join(root, "bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s published bin: %v", name, err)
		}
	}
	mustStart(t, root, &fakeServer{set: a})
	if executable, err := start(root, &fakeServer{set: b, serve: map[string]string{"claude": forged(b["claude"])}}); err == nil || executable != "" {
		t.Fatalf("wrong digest over an active release accepted: %q %v", executable, err)
	}
	assertActive(t, root, a)
	if got := releaseEntries(t, root); !slices.Equal(got, []string{a.id(t)}) {
		t.Fatalf("releases %v", got)
	}
}

// TestReleaseRefusesUnsafeLayout: корень, releases или lock, которые не
// принадлежат процессу, открыты на запись другим или являются ссылками:
// отказ до первой записи; ссылки не разыменовываются.
func TestReleaseRefusesUnsafeLayout(t *testing.T) {
	a := newReleaseSet("a")
	cases := map[string]func(t *testing.T, root, outside string){
		"root is a link": func(t *testing.T, root, outside string) { symlink(t, outside, root) },
		"root writable by others": func(t *testing.T, root, _ string) {
			mkdir(t, root)
			chmod(t, root, 0o777)
		},
		"root writable by group": func(t *testing.T, root, _ string) {
			mkdir(t, root)
			chmod(t, root, 0o775)
		},
		"releases is a link": func(t *testing.T, root, outside string) {
			mkdir(t, root)
			symlink(t, outside, filepath.Join(root, "releases"))
		},
		"releases writable by others": func(t *testing.T, root, _ string) {
			mkdir(t, root)
			mkdir(t, filepath.Join(root, "releases"))
			chmod(t, filepath.Join(root, "releases"), 0o777)
		},
		"lock is a link": func(t *testing.T, root, outside string) {
			mkdir(t, root)
			writeFile(t, filepath.Join(outside, "file"), "data")
			symlink(t, filepath.Join(outside, "file"), filepath.Join(root, ".lock"))
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			root, outside := newRoot(t), t.TempDir()
			// Цель ссылки сама по себе доверенная: отказ вызывает только ссылка.
			chmod(t, outside, 0o755)
			prepare(t, root, outside)
			before := snapshotTree(t, outside, "")
			executable, err := start(root, &fakeServer{set: a})
			if err == nil || executable != "" || !strings.HasPrefix(err.Error(), "guest release ") {
				t.Fatalf("unsafe layout accepted: %q %v", executable, err)
			}
			if after := snapshotTree(t, outside, ""); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused start changed the link target:\n%v\n%v", before, after)
			}
		})
	}
	// Каталоги root без доступа остальным (крах между mkdir и chmod при
	// строгом umask) получают 0755: иначе актор не доберётся до bin/claude.
	t.Run("closed directories are opened for the actor", func(t *testing.T) {
		root := newRoot(t)
		mkdir(t, root)
		mkdir(t, filepath.Join(root, "releases"))
		chmod(t, filepath.Join(root, "releases"), 0o700)
		chmod(t, root, 0o700)
		mustStart(t, root, &fakeServer{set: a})
		assertActive(t, root, a)
		for _, dir := range []string{root, filepath.Join(root, "releases")} {
			if info, _ := os.Lstat(dir); info.Mode().Perm() != 0o755 {
				t.Fatalf("%s mode %v, want 0755", dir, info.Mode())
			}
		}
	})
	t.Run("foreign bin link is replaced, not followed", func(t *testing.T) {
		root, outside := newRoot(t), t.TempDir()
		writeSet(t, outside, newReleaseSet("outside"))
		mkdir(t, root)
		symlink(t, outside, filepath.Join(root, "bin"))
		mustStart(t, root, &fakeServer{set: a})
		assertActive(t, root, a)
		if got := readSet(t, outside); !reflect.DeepEqual(got, newReleaseSet("outside")) {
			t.Fatalf("bin link target changed: %v", got)
		}
	})
}

// TestReleaseIgnoresFIFO: FIFO под именем артефакта (или как запись
// releases) не подвешивает bootstrap: набор считается повреждённым и
// качается заново.
func TestReleaseIgnoresFIFO(t *testing.T) {
	root := newRoot(t)
	a := newReleaseSet("a")
	mustStart(t, root, &fakeServer{set: a})
	claude := filepath.Join(root, "releases", a.id(t), "claude")
	remove(t, claude)
	if err := syscall.Mkfifo(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "releases", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	server := &fakeServer{set: a}
	go func() {
		_, err := start(root, server)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bootstrap hangs on a FIFO")
	}
	if !slices.Equal(server.downloads, releaseNames) {
		t.Fatalf("release with a FIFO reused: %v", server.downloads)
	}
	assertActive(t, root, a)
	if got := releaseEntries(t, root); !slices.Equal(got, []string{a.id(t)}) {
		t.Fatalf("releases %v", got)
	}
}

// TestReleaseConcurrentStarts: два bootstrap одного корня не мешают друг
// другу: оба набора проверены, bin указывает на последний, staging нет.
func TestReleaseConcurrentStarts(t *testing.T) {
	root := newRoot(t)
	a, b := newReleaseSet("a"), newReleaseSet("b")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, set := range []releaseSet{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server := &fakeServer{set: set}
			_, errs[i] = installRelease(context.Background(), root, set.manifest(), server.download, func(error) {})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if got := releaseEntries(t, root); !slices.Equal(got, sortedIDs(t, a, b)) {
		t.Fatalf("releases %v", got)
	}
	current, previous := linkTarget(t, filepath.Join(root, "bin")), linkTarget(t, filepath.Join(root, "previous"))
	pair := []string{current, previous}
	sort.Strings(pair)
	if want := []string{"releases/" + sortedIDs(t, a, b)[0], "releases/" + sortedIDs(t, a, b)[1]}; !slices.Equal(pair, want) {
		t.Fatalf("bin %q previous %q", current, previous)
	}
	for _, set := range []releaseSet{a, b} {
		if got := readSet(t, filepath.Join(root, "releases", set.id(t))); !reflect.DeepEqual(got, set) {
			t.Fatalf("release %s content %v", set.id(t), got)
		}
	}
}

// TestReleaseLeavesOutsideUntouched: установка, повторное использование,
// миграция, отказ, откат и уборка не меняют ничего вне корня артефактов:
// ни home и workspace, ни bootstrap образа, ни цель ссылки в корень.
func TestReleaseLeavesOutsideUntouched(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "opt", "brood")
	writeSet(t, filepath.Join(root, "bin"), newReleaseSet("old"))
	writeFile(t, filepath.Join(base, "workspace", "home", ".claude", "settings.json"), `{"theme":"dark"}`)
	writeFile(t, filepath.Join(base, "workspace", "home", ".bash_history"), "make test\n")
	writeFile(t, filepath.Join(base, "workspace", "project", ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(base, "workspace", "project", "main.go"), "package main\n")
	writeFile(t, filepath.Join(base, "usr", "local", "bin", "brood-guest"), "\x7fELFbootstrap")
	symlink(t, "../../opt/brood/bin", filepath.Join(base, "workspace", "home", "tools"))
	before := snapshotTree(t, base, root)
	a, b, c := newReleaseSet("a"), newReleaseSet("b"), newReleaseSet("c")
	mustStart(t, root, &fakeServer{set: a})
	mustStart(t, root, &fakeServer{set: a, forbid: t})
	mustStart(t, root, &fakeServer{set: b})
	if _, err := start(root, &fakeServer{set: c, broken: "claude"}); err == nil {
		t.Fatal("broken download accepted")
	}
	writeFile(t, filepath.Join(root, "releases", b.id(t), "bro"), "\x7fELFdamaged")
	mustStart(t, root, &fakeServer{set: b})
	mustStart(t, root, &fakeServer{set: c})
	mustStart(t, root, &fakeServer{set: b, forbid: t})
	if after := snapshotTree(t, base, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("outside of the artifact root changed:\n%v\n%v", before, after)
	}
}

// snapshotTree описывает дерево dir без поддерева skip: тип, права, время
// изменения, содержимое файлов и цели ссылок.
func snapshotTree(t *testing.T, dir, skip string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == skip {
			return filepath.SkipDir
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%v %d", info.Mode(), info.ModTime().UnixNano())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			line += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			line += " " + hex.EncodeToString(sum[:])
		}
		snapshot[path] = line
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	chmod(t, path, 0o755)
}

func chmod(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
