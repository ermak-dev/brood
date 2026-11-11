//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"brood.dev/go/brood/proxmox/guest"
)

// Раскладка корня артефактов (#184), в CT это /opt/brood:
//
//	releases/<id>/{bro,brood-guest,claude}  проверенные наборы, id из ReleaseID манифеста
//	releases/.stage-<id>                    незаконченная установка, не используется
//	bin -> releases/<id>                    текущий набор: прежние пути /opt/brood/bin/*
//	previous -> releases/<id>               прежний набор, хранится для отката
//	bin.legacy-<n>                          плоский bin прежнего bootstrap, отодвинут
//	.lock                                   flock: один bootstrap за раз
//
// Пишет только root bootstrap-стадии, до runtime-стадии и процессов актора,
// поэтому переключение bin никто не видит посреди работы. Все операции идут
// от дескрипторов проверенных каталогов и не следуют ссылкам.
const (
	releasesName = "releases"
	binName      = "bin"
	previousName = "previous"
	lockName     = ".lock"
	stagePrefix  = ".stage-"
	legacyPrefix = "bin.legacy-"
	// removeDepth ограничивает рекурсию уборки с запасом: набор плоский,
	// глубже бывает только созданное root вручную.
	removeDepth = 64
)

// Причины отказа статичны: их печатает bootstrap в консоль CT.
var (
	errReleaseManifest = errors.New("guest release manifest rejected")
	errReleaseUnsafe   = errors.New("guest release root unsafe")
	errReleaseStore    = errors.New("guest release store unavailable")
	errReleaseLock     = errors.New("guest release lock unavailable")
	errReleaseDownload = errors.New("guest release artifacts not delivered")
	errReleaseSwitch   = errors.New("guest release switch failed")
	errReleaseCleanup  = errors.New("guest release cleanup failed")
	errReleaseLegacy   = errors.New("guest release cannot move the plain bin aside")
)

// installRelease делает набор манифеста текущим в корне root и возвращает
// путь его brood-guest для runtime-стадии. Существующий releases/<id>
// проверяется заново (sha256 каждого файла) и используется без загрузки;
// отсутствующий или повреждённый качается через download в staging и
// публикуется только целиком. Затем bin атомарно переключается на набор,
// прежний остаётся в previous, остальное убирается: после успеха на диске
// не больше двух наборов. Отказ уборки старт не останавливает, а уходит в
// warn: bin указывает на проверенный набор, а отказ повторялся бы на каждом
// старте и выводил актора из строя навсегда. Остаток прерванной установки,
// который мешает загрузке, остановит её собственным отказом.
func installRelease(ctx context.Context, root string, manifest guest.ArtifactManifest, download guest.ArtifactDownload, warn func(error)) (string, error) {
	id, err := manifest.ReleaseID()
	if err != nil {
		return "", errReleaseManifest
	}
	s, err := openReleaseStore(ctx, root)
	if err != nil {
		return "", err
	}
	defer s.close()
	current, err := s.current()
	if err != nil {
		return "", err
	}
	// Уборка до загрузки освобождает место и снимает staging прерванного старта.
	if err := s.collect(id, current, releaseLink(s.root, previousName)); err != nil {
		warn(err)
	}
	if err := s.ensure(ctx, id, manifest, download); err != nil {
		return "", err
	}
	if err := s.activate(id, current); err != nil {
		return "", err
	}
	if err := s.collect(id, releaseLink(s.root, previousName)); err != nil {
		warn(err)
	}
	return filepath.Join(root, releasesName, id, "brood-guest"), nil
}

type releaseStore struct {
	root, lock, releases int
}

// openReleaseStore открывает корень и releases, создавая их 0755, и берёт
// flock. Корень и releases должны быть каталогами этого процесса (root в CT),
// закрытыми на запись группе и остальным, а не ссылками.
func openReleaseStore(ctx context.Context, path string) (*releaseStore, error) {
	s := &releaseStore{root: -1, lock: -1, releases: -1}
	var err error
	if s.root, err = openOrCreateDir(unix.AT_FDCWD, path); err != nil {
		return nil, err
	}
	if err = s.acquire(ctx); err != nil {
		s.close()
		return nil, err
	}
	if s.releases, err = openOrCreateDir(s.root, releasesName); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *releaseStore) close() {
	for _, fd := range []int{s.releases, s.lock, s.root} {
		if fd >= 0 {
			unix.Close(fd)
		}
	}
}

// acquire ждёт flock на .lock, пока жив ctx: второй bootstrap того же корня
// (ручной запуск root) не пересекается с первым.
func (s *releaseStore) acquire(ctx context.Context) error {
	fd, err := unix.Openat(s.root, lockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return errReleaseUnsafe
		}
		return errReleaseLock
	}
	s.lock = fd
	if !trusted(fd, unix.S_IFREG) {
		return errReleaseUnsafe
	}
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return errReleaseLock
		}
		select {
		case <-ctx.Done():
			return errReleaseLock
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// current: id набора, на который указывает bin; "" значит набора нет. bin, не
// являющийся ссылкой (плоский каталог прежнего bootstrap), отодвигается
// переименованием, а не удаляется; ссылка не на набор просто заменится.
func (s *releaseStore) current() (string, error) {
	var st unix.Stat_t
	err := unix.Fstatat(s.root, binName, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return "", nil
	}
	if err != nil {
		return "", errReleaseStore
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return "", s.moveAside()
	}
	return releaseLink(s.root, binName), nil
}

// moveAside переименовывает bin в первое свободное bin.legacy-<n>.
func (s *releaseStore) moveAside() error {
	for n := 1; n <= 64; n++ {
		name := legacyPrefix + strconv.Itoa(n)
		var st unix.Stat_t
		err := unix.Fstatat(s.root, name, &st, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			continue
		}
		if !errors.Is(err, unix.ENOENT) || unix.Renameat(s.root, binName, s.root, name) != nil || unix.Fsync(s.root) != nil {
			return errReleaseLegacy
		}
		return nil
	}
	return errReleaseLegacy
}

// ensure делает releases/<id> целым набором манифеста.
func (s *releaseStore) ensure(ctx context.Context, id string, manifest guest.ArtifactManifest, download guest.ArtifactDownload) error {
	var st unix.Stat_t
	err := unix.Fstatat(s.releases, id, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil && s.intact(id, manifest) {
		return nil
	}
	if !errors.Is(err, unix.ENOENT) {
		// Повреждённый или неполный набор не используется: под его именем
		// не остаётся ничего, а частично удалённый проверку не пройдёт.
		if removeAt(s.releases, id, removeDepth) != nil {
			return errReleaseCleanup
		}
	}
	return s.install(ctx, id, manifest, download)
}

// intact заново проверяет releases/<id>: каталог 0755 этого процесса ровно с
// файлами манифеста, каждый из них обычный файл 0755 этого процесса с размером и
// sha256 записи. Полное хеширование (доли секунды на 310 МБ плюс чтение с
// диска) выбрано вместо отметки об установке: отметка не ловит порчу диска и
// правку файлов root.
func (s *releaseStore) intact(id string, manifest guest.ArtifactManifest) bool {
	dir, err := unix.Openat(s.releases, id, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(dir)
	names, err := listDir(dir)
	if err != nil || !sealed(dir, unix.S_IFDIR) || len(names) != len(manifest.Artifacts) {
		return false
	}
	for _, artifact := range manifest.Artifacts {
		if !intactArtifact(dir, artifact) {
			return false
		}
	}
	return true
}

func intactArtifact(dir int, artifact guest.Artifact) bool {
	// O_NONBLOCK: FIFO под именем артефакта не подвешивает bootstrap.
	fd, err := unix.Openat(dir, artifact.Name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), artifact.Name)
	defer file.Close()
	if !sealed(fd, unix.S_IFREG) {
		return false
	}
	digest := sha256.New()
	read, err := io.Copy(digest, io.LimitReader(file, artifact.Size+1))
	return err == nil && read == artifact.Size && hex.EncodeToString(digest.Sum(nil)) == artifact.SHA256
}

// install качает набор в releases/.stage-<id> и публикует его rename-ом в
// releases/<id> только целиком: каждый файл сверен с манифестом, записан на
// диск и закрыт на запись. Оборванную установку удаляет install, оставленную
// крахом убирает следующий старт; под именем набора её не бывает.
func (s *releaseStore) install(ctx context.Context, id string, manifest guest.ArtifactManifest, download guest.ArtifactDownload) (err error) {
	stageName := stagePrefix + id
	if unix.Mkdirat(s.releases, stageName, 0o700) != nil {
		return errReleaseStore
	}
	stage, openErr := unix.Openat(s.releases, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	defer func() {
		if stage >= 0 {
			unix.Close(stage)
		}
		if err != nil {
			removeAt(s.releases, stageName, removeDepth)
		}
	}()
	if openErr != nil {
		return errReleaseStore
	}
	installed := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		if download(ctx, artifact, stageInstaller(stage, artifact, installed)) != nil || !installed[artifact.Name] {
			return errReleaseDownload
		}
	}
	if unix.Fchmod(stage, 0o755) != nil || unix.Fsync(stage) != nil {
		return errReleaseStore
	}
	if unix.Renameat(s.releases, stageName, s.releases, id) != nil || unix.Fsync(s.releases) != nil {
		return errReleaseStore
	}
	return nil
}

// stageInstaller пишет артефакт want в staging: спул .<name>.part ровно из
// size байт, сверка sha256 с манифестом, 0755, fsync и только затем rename в
// имя артефакта. Поток с другим digest имени артефакта не получает (#265).
func stageInstaller(stage int, want guest.Artifact, installed map[string]bool) guest.ArtifactInstaller {
	return func(_ context.Context, name string, stream io.Reader, size int64, digest string) error {
		if name != want.Name || size != want.Size || digest != want.SHA256 {
			return errors.New("guest artifact does not match the manifest")
		}
		spool := "." + name + ".part"
		fd, err := unix.Openat(stage, spool, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return errors.New("guest artifact staging unavailable")
		}
		file := os.NewFile(uintptr(fd), spool)
		defer file.Close()
		sum := sha256.New()
		written, err := io.Copy(io.MultiWriter(file, sum), io.LimitReader(stream, size+1))
		if err != nil || written != size {
			return errors.New("guest artifact stream invalid")
		}
		if hex.EncodeToString(sum.Sum(nil)) != digest {
			return errors.New("guest artifact digest mismatch")
		}
		if file.Chmod(0o755) != nil || file.Sync() != nil || file.Close() != nil {
			return errors.New("guest artifact sync failed")
		}
		if unix.Renameat(stage, spool, stage, name) != nil {
			return errors.New("guest artifact publish failed")
		}
		installed[name] = true
		return nil
	}
}

// activate переключает bin на releases/<id>. Каждая ссылка ставится
// атомарно: новая ссылка рядом и rename поверх, поэтому /opt/brood/bin
// всегда указывает на целый набор. previous записывается раньше bin: крах
// между шагами оставляет bin на прежнем наборе, а следующий старт повторит.
func (s *releaseStore) activate(id, current string) error {
	for _, name := range []string{binName, previousName} {
		if err := unix.Unlinkat(s.root, nextLink(name), 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return errReleaseSwitch
		}
	}
	if current == id {
		return nil
	}
	if current != "" {
		if err := s.link(previousName, current); err != nil {
			return err
		}
	}
	return s.link(binName, id)
}

func nextLink(name string) string { return "." + name + ".next" }

// link атомарно направляет name на releases/<id> и фиксирует это fsync корня.
func (s *releaseStore) link(name, id string) error {
	next := nextLink(name)
	if unix.Symlinkat(releasesName+"/"+id, s.root, next) != nil || unix.Renameat(s.root, next, s.root, name) != nil || unix.Fsync(s.root) != nil {
		return errReleaseSwitch
	}
	return nil
}

// collect удаляет из releases всё, кроме наборов keep: прежние наборы,
// staging прерванных стартов и посторонние записи. Пустые keep не значат
// ничего; ссылки удаляются сами, их цели не трогаются. Запись, которую
// удалить не вышло, не мешает убрать остальные.
func (s *releaseStore) collect(keep ...string) error {
	names, err := listDir(s.releases)
	if err != nil {
		return errReleaseCleanup
	}
	failed := false
	for _, name := range names {
		if slices.Contains(keep, name) {
			continue
		}
		if removeAt(s.releases, name, removeDepth) != nil {
			failed = true
		}
	}
	if failed {
		return errReleaseCleanup
	}
	return nil
}

// releaseLink: id набора из ссылки name вида releases/<id>; иначе "".
func releaseLink(dir int, name string) string {
	buf := make([]byte, 128)
	n, err := unix.Readlinkat(dir, name, buf)
	if err != nil || n >= len(buf) {
		return ""
	}
	id, ok := strings.CutPrefix(string(buf[:n]), releasesName+"/")
	if !ok || !validReleaseID(id) {
		return ""
	}
	return id
}

func validReleaseID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// openOrCreateDir открывает каталог name в parent без следования ссылкам,
// создавая его при отсутствии. Каталог чужого владельца, открытый на запись
// группе или остальным, или ссылка: отказ. Доверенный каталог получает ровно
// 0755: ни umask, ни крах между mkdir и chmod не решают, доберётся ли актор
// до bro и claude.
func openOrCreateDir(parent int, name string) (int, error) {
	if err := unix.Mkdirat(parent, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, errReleaseStore
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, errReleaseUnsafe
		}
		return -1, errReleaseStore
	}
	if !trusted(fd, unix.S_IFDIR) {
		unix.Close(fd)
		return -1, errReleaseUnsafe
	}
	if !sealed(fd, unix.S_IFDIR) && unix.Fchmod(fd, 0o755) != nil {
		unix.Close(fd)
		return -1, errReleaseStore
	}
	return fd, nil
}

// trusted: fd нужного типа, принадлежит этому процессу и закрыт на запись
// группе и остальным, то есть UID 1000 не может его менять.
func trusted(fd int, kind uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == kind && int(st.Uid) == os.Geteuid() && st.Mode&0o022 == 0
}

// sealed: как trusted, и права ровно 0755 без setuid/setgid/sticky.
func sealed(fd int, kind uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == kind && int(st.Uid) == os.Geteuid() && st.Mode&0o7777 == 0o755
}

// listDir: имена записей каталога dir без . и ...
func listDir(dir int) ([]string, error) {
	fd, err := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), ".")
	defer file.Close()
	return file.Readdirnames(-1)
}

// removeAt удаляет name в dir: ссылку или файл через unlinkat, каталог
// вместе с содержимым через дескрипторы, без следования ссылкам.
func removeAt(dir int, name string, depth int) error {
	err := unix.Unlinkat(dir, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) || depth == 0 {
		return err
	}
	child, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(child)
	names, err := listDir(child)
	if err != nil {
		return err
	}
	for _, entry := range names {
		if err := removeAt(child, entry, depth-1); err != nil {
			return err
		}
	}
	return unix.Unlinkat(dir, name, unix.AT_REMOVEDIR)
}
