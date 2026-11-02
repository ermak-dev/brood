//go:build linux

package guest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"brood.dev/go/brood/projectsecret"
	"golang.org/x/sys/unix"
)

// openSecretDirectory walks every component with O_NOFOLLOW, including ancestors.
// Pinned directory descriptors prevent symlink substitution during later writes.
func openSecretDirectory(path string, create bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, secretFailure()
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, secretFailure()
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := secretChildDirectory(fd, part, create)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}
func secretChildDirectory(fd int, name string, create bool) (int, error) {
	if create {
		if err := unix.Mkdirat(fd, name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, secretFailure()
		}
	}
	next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	return next, nil
}
func secureSecretDirectory(fd, uid int, private bool) error {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(uid) || st.Mode&0022 != 0 {
		return secretFailure()
	}
	if private && st.Mode&0077 != 0 {
		return secretFailure()
	}
	return nil
}
func readSecretFile(dir int, name string, limit, uid int, mode uint32) ([]byte, unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, st, err
	}
	file := os.NewFile(uintptr(fd), "project secret")
	defer file.Close()
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != uint32(uid) || st.Size > int64(limit) || (mode != 0 && st.Mode&0777 != mode) {
		return nil, st, secretFailure()
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(data) > limit {
		return nil, st, secretFailure()
	}
	return data, st, nil
}
func secretTempName() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", secretFailure()
	}
	return ".brood-secret-" + hex.EncodeToString(data[:]), nil
}
func writeSecretTemp(dir int, data []byte, mode uint32, uid, gid int) (string, error) {
	name, err := secretTempName()
	if err != nil {
		return "", err
	}
	return writeNamedSecretTemp(dir, name, data, mode, uid, gid)
}

// An unnamed inode cannot leave partially written secret bytes after a crash.
// Its journaled name becomes visible only after the complete inode is synced.
func writeNamedSecretTemp(dir int, name string, data []byte, mode uint32, uid, gid int) (string, error) {
	fd, err := unix.Openat(dir, ".", unix.O_WRONLY|unix.O_TMPFILE|unix.O_CLOEXEC, 0600)
	if err != nil {
		return "", secretFailure()
	}
	file := os.NewFile(uintptr(fd), "project secret")
	defer file.Close()
	if uid != os.Geteuid() || gid != os.Getegid() {
		if unix.Fchown(fd, uid, gid) != nil {
			return "", secretFailure()
		}
	}
	if _, err = file.Write(data); err != nil {
		return "", secretFailure()
	}
	if unix.Fchmod(fd, mode) != nil || file.Sync() != nil {
		return "", secretFailure()
	}
	if unix.Linkat(fd, "", dir, name, unix.AT_EMPTY_PATH) != nil {
		return "", secretFailure()
	}
	return name, nil
}
func replaceSecretFile(dir int, name string, data []byte, mode uint32, uid, gid int, noReplace bool) error {
	temp, err := writeSecretTemp(dir, data, mode, uid, gid)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(dir, temp, 0)
	flags := uint(0)
	if noReplace {
		flags = unix.RENAME_NOREPLACE
	}
	if unix.Renameat2(dir, temp, dir, name, flags) != nil || unix.Fsync(dir) != nil {
		return secretFailure()
	}
	return nil
}
func secretHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// secretManifest contains only relative names and hashes, never values or vars.
// It lives outside git-clean's scope. A missing file after recycle is expected.
type secretManifest map[string]string

func secretManifestDirectory(home string) (int, error) {
	fd, err := openSecretDirectory(home, false)
	if err != nil {
		return -1, secretFailure()
	}
	for _, name := range []string{".config", "brood"} {
		child, err := secretChildDirectory(fd, name, true)
		unix.Close(fd)
		if err != nil {
			return -1, secretFailure()
		}
		if secureSecretDirectory(child, os.Geteuid(), false) != nil {
			unix.Close(child)
			return -1, secretFailure()
		}
		fd = child
	}
	if secureSecretDirectory(fd, os.Geteuid(), true) != nil {
		unix.Close(fd)
		return -1, secretFailure()
	}
	return fd, nil
}
func readSecretManifest(dir int) (secretManifest, error) {
	data, _, err := readSecretFile(dir, "project-secrets.json", 64<<10, os.Geteuid(), 0600)
	if errors.Is(err, os.ErrNotExist) {
		return secretManifest{}, nil
	}
	if err != nil {
		return nil, secretFailure()
	}
	var manifest secretManifest
	if json.Unmarshal(data, &manifest) != nil || len(manifest) > secretManagedLimit {
		return nil, secretFailure()
	}
	for name, hash := range manifest {
		raw, err := hex.DecodeString(hash)
		if projectsecret.ValidateFileName(name) != nil || err != nil || len(raw) != sha256.Size {
			return nil, secretFailure()
		}
	}
	if manifest == nil {
		manifest = secretManifest{}
	}
	return manifest, nil
}
func secretParent(root int, name string, create bool) (int, string, error) {
	parts := strings.Split(name, "/")
	fd, err := unix.Dup(root)
	if err != nil {
		return -1, "", secretFailure()
	}
	unix.CloseOnExec(fd)
	for _, part := range parts[:len(parts)-1] {
		child, err := secretChildDirectory(fd, part, create)
		unix.Close(fd)
		if err != nil {
			return -1, "", err
		}
		fd = child
	}
	return fd, parts[len(parts)-1], nil
}
func managedSecretHash(root int, name string) (string, error) {
	parent, base, err := secretParent(root, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", secretFailure()
	}
	defer unix.Close(parent)
	data, _, err := readSecretFile(parent, base, projectsecret.MaxFileBytes, os.Geteuid(), 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", secretFailure()
	}
	return secretHash(data), nil
}

const secretJournalName = "project-secrets-journal.json"
const secretJournalLimit = 256 << 10
const secretManagedLimit = 4 * projectsecret.MaxFiles

// The write-ahead journal contains only relative paths and hashes. Both sides
// remain owned until recovery commits the actually installed subset. Scratch
// names are stable per destination, so repeated launches do not grow excludes.
type secretJournalEntry struct {
	Old    string `json:"old"`
	New    string `json:"new"`
	Backup string `json:"backup"`
	Temp   string `json:"temp"`
}
type secretJournal map[string]secretJournalEntry

func secretScratch(name, suffix string) string {
	return filepath.Join(filepath.Dir(name), ".brood-secret-"+secretHash([]byte(name))[:32]+suffix)
}
func validSecretHash(hash string) bool {
	raw, err := hex.DecodeString(hash)
	return err == nil && len(raw) == sha256.Size && strings.ToLower(hash) == hash
}
func readSecretJournal(dir int) (secretJournal, error) {
	data, _, err := readSecretFile(dir, secretJournalName, secretJournalLimit, os.Geteuid(), 0600)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, secretFailure()
	}
	var journal secretJournal
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&journal) != nil || decoder.Decode(new(any)) != io.EOF || journal == nil || len(journal) > secretManagedLimit {
		return nil, secretFailure()
	}
	used := map[string]bool{}
	for name := range journal {
		used[name] = true
	}
	for name, entry := range journal {
		if projectsecret.ValidateFileName(name) != nil || (entry.Old == "" && entry.New == "") || (entry.Old != "" && !validSecretHash(entry.Old)) || (entry.New != "" && !validSecretHash(entry.New)) || entry.Backup != secretScratch(name, "-old") || entry.Temp != secretScratch(name, "-new") || used[entry.Backup] || used[entry.Temp] {
			return nil, secretFailure()
		}
		used[entry.Backup], used[entry.Temp] = true, true
	}
	return journal, nil
}

// flock serializes separate processes as well as goroutines. Never unlink the
// lock: doing so would let a later writer lock a different inode.
func lockSecretManifest(dir int) (int, error) {
	fd, err := unix.Openat(dir, "project-secrets.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return -1, secretFailure()
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0600 || st.Size != 0 || unix.Flock(fd, unix.LOCK_EX) != nil {
		unix.Close(fd)
		return -1, secretFailure()
	}
	return fd, nil
}
func writeSecretManifest(dir int, manifest secretManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil || len(data) > 64<<10 || len(manifest) > secretManagedLimit {
		return secretFailure()
	}
	return replaceSecretFile(dir, "project-secrets.json", data, 0600, os.Geteuid(), os.Getegid(), false)
}
func finishSecretJournal(dir int) error {
	if unix.Unlinkat(dir, secretJournalName, 0) != nil || unix.Fsync(dir) != nil {
		return secretFailure()
	}
	return nil
}
func removeSecretScratch(root int, name, hash string) error {
	got, err := managedSecretHash(root, name)
	if err != nil || (got != "" && (hash == "" || got != hash)) {
		return secretFailure()
	}
	if got == "" {
		return nil
	}
	dir, base, err := secretParent(root, name, false)
	if err != nil {
		return secretFailure()
	}
	defer unix.Close(dir)
	if unix.Unlinkat(dir, base, 0) != nil || unix.Fsync(dir) != nil {
		return secretFailure()
	}
	return nil
}
func recoverSecretJournal(root, dir int, manifest secretManifest) (secretManifest, error) {
	journal, err := readSecretJournal(dir)
	if err != nil || journal == nil {
		return manifest, err
	}
	actual := secretManifest{}
	for name, hash := range manifest {
		entry, ok := journal[name]
		if !ok || (hash != entry.Old && hash != entry.New) {
			return nil, secretFailure()
		}
	}
	// Check the whole transaction before deleting any owned scratch files.
	for name, entry := range journal {
		got, err := managedSecretHash(root, name)
		if err != nil || (got != "" && got != entry.Old && got != entry.New) {
			return nil, secretFailure()
		}
		if got != "" {
			actual[name] = got
		}
		for path, hash := range map[string]string{entry.Backup: entry.Old, entry.Temp: entry.New} {
			got, err := managedSecretHash(root, path)
			if err != nil || (got != "" && (hash == "" || got != hash)) {
				return nil, secretFailure()
			}
		}
	}
	for _, entry := range journal {
		if removeSecretScratch(root, entry.Backup, entry.Old) != nil || removeSecretScratch(root, entry.Temp, entry.New) != nil {
			return nil, secretFailure()
		}
	}
	if writeSecretManifest(dir, actual) != nil || finishSecretJournal(dir) != nil {
		return nil, secretFailure()
	}
	return actual, nil
}

// A checkpoint is an optional deterministic interruption boundary used by tests;
// production has no hook. Every file effect is covered by the durable journal.
type secretCheckpoint func(string) error

func (step secretCheckpoint) at(name string) error {
	if step != nil {
		return step(name)
	}
	return nil
}
func changeManagedSecret(root int, name string, entry secretJournalEntry, data []byte, step secretCheckpoint) error {
	dir, base, err := secretParent(root, name, entry.New != "")
	if entry.New == "" && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return secretFailure()
	}
	defer unix.Close(dir)
	backup, temp := filepath.Base(entry.Backup), filepath.Base(entry.Temp)
	if entry.New != "" {
		if _, err := writeNamedSecretTemp(dir, temp, data, 0600, os.Geteuid(), os.Getegid()); err != nil {
			return err
		}
		if unix.Fsync(dir) != nil {
			return secretFailure()
		}
		if err := step.at("prepared:" + name); err != nil {
			return err
		}
	}
	moved := false
	if err := unix.Renameat2(dir, base, dir, backup, unix.RENAME_NOREPLACE); err == nil {
		moved = true
		previous, _, readErr := readSecretFile(dir, backup, projectsecret.MaxFileBytes, os.Geteuid(), 0)
		if readErr != nil || entry.Old == "" || secretHash(previous) != entry.Old {
			_ = unix.Renameat2(dir, backup, dir, base, unix.RENAME_NOREPLACE)
			return secretFailure()
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return secretFailure()
	}
	if unix.Fsync(dir) != nil {
		return secretFailure()
	}
	if err := step.at("displaced:" + name); err != nil {
		return err
	}
	if entry.New != "" {
		if unix.Renameat2(dir, temp, dir, base, unix.RENAME_NOREPLACE) != nil {
			if moved {
				_ = unix.Renameat2(dir, backup, dir, base, unix.RENAME_NOREPLACE)
			}
			return secretFailure()
		}
	}
	if unix.Fsync(dir) != nil {
		return secretFailure()
	}
	if err := step.at("installed:" + name); err != nil {
		return err
	}
	if moved && unix.Unlinkat(dir, backup, 0) != nil {
		return secretFailure()
	}
	if unix.Fsync(dir) != nil {
		return secretFailure()
	}
	return step.at("cleaned:" + name)
}

func applyProjectSecrets(project, home string, bundle projectsecret.Bundle) ([]string, error) {
	return applyProjectSecretsWithCheckpoint(project, home, bundle, nil)
}
func applyProjectSecretsWithCheckpoint(project, home string, bundle projectsecret.Bundle, step secretCheckpoint) ([]string, error) {
	env, err := projectAppEnvironment(bundle)
	if err != nil {
		return nil, err
	}
	root, err := openSecretDirectory(project, false)
	if err != nil {
		return nil, secretFailure()
	}
	defer unix.Close(root)
	manifestDir, err := secretManifestDirectory(home)
	if err != nil {
		return nil, err
	}
	defer unix.Close(manifestDir)
	lock, err := lockSecretManifest(manifestDir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(lock)
	manifest, err := readSecretManifest(manifestDir)
	if err != nil {
		return nil, err
	}
	manifest, err = recoverSecretJournal(root, manifestDir, manifest)
	if err != nil {
		return nil, err
	}
	next := secretManifest{}
	files := map[string][]byte{}
	for _, file := range bundle.Files {
		next[file.Name], files[file.Name] = secretHash(file.Data), file.Data
	}
	data, err := json.Marshal(next)
	if err != nil || len(data) > 64<<10 {
		return nil, secretFailure()
	}
	journal := secretJournal{}
	for name, hash := range manifest {
		journal[name] = secretJournalEntry{Old: hash}
	}
	for name, hash := range next {
		entry := journal[name]
		entry.New = hash
		journal[name] = entry
	}
	if len(journal) > secretManagedLimit {
		return nil, secretFailure()
	}
	// Any interrupted old/new mixture must itself fit the manifest reader.
	// Otherwise a valid journal could become impossible to retire on recovery.
	union := secretManifest{}
	for name, entry := range journal {
		hash := entry.New
		if hash == "" {
			hash = entry.Old
		}
		union[name] = hash
	}
	unionData, err := json.Marshal(union)
	if err != nil || len(unionData) > 64<<10 {
		return nil, secretFailure()
	}
	ordered := make([]string, 0, len(journal))
	ignored := make([]projectsecret.File, 0, 3*len(journal))
	for name, entry := range journal {
		entry.Backup, entry.Temp = secretScratch(name, "-old"), secretScratch(name, "-new")
		journal[name] = entry
		ordered = append(ordered, name)
		if entry.New != "" {
			ignored = append(ignored, projectsecret.File{Name: name})
		}
		ignored = append(ignored, projectsecret.File{Name: entry.Backup}, projectsecret.File{Name: entry.Temp})
	}
	sort.Strings(ordered)
	// Validate destinations and all scratch names before publishing any bytes.
	for _, name := range ordered {
		entry := journal[name]
		got, err := managedSecretHash(root, name)
		if err != nil || (got != "" && got != entry.Old) {
			return nil, secretFailure()
		}
		for _, scratch := range []string{entry.Backup, entry.Temp} {
			if _, exists := journal[scratch]; exists {
				return nil, secretFailure()
			}
			got, err := managedSecretHash(root, scratch)
			if err != nil || got != "" {
				return nil, secretFailure()
			}
		}
	}
	journalData, err := json.Marshal(journal)
	if err != nil || len(journalData) > secretJournalLimit {
		return nil, secretFailure()
	}
	if err := ignoreProjectSecrets(root, ignored); err != nil {
		return nil, err
	}
	if err := replaceSecretFile(manifestDir, secretJournalName, journalData, 0600, os.Geteuid(), os.Getegid(), false); err != nil {
		return nil, err
	}
	if err := step.at("journal"); err != nil {
		return nil, err
	}
	for _, name := range ordered {
		if err := changeManagedSecret(root, name, journal[name], files[name], step); err != nil {
			return nil, err
		}
	}
	if err := writeSecretManifest(manifestDir, next); err != nil {
		return nil, err
	}
	if err := step.at("manifest"); err != nil {
		return nil, err
	}
	if err := finishSecretJournal(manifestDir); err != nil {
		return nil, err
	}
	return env, nil
}

// ignoreProjectSecrets preserves user .gitignore and existing excludes. Metadata
// paths are descriptor-walked too, and only path patterns (not values) are added.
func ignoreProjectSecrets(root int, files []projectsecret.File) error {
	if len(files) == 0 {
		return nil
	}
	// File delivery requires an existing checkout: a later git init would not
	// inherit excludes and would expose previously delivered secret bytes.
	git, err := secretChildDirectory(root, ".git", false)
	if err != nil {
		return secretFailure()
	}
	defer unix.Close(git)
	info, err := secretChildDirectory(git, "info", true)
	if err != nil {
		return secretFailure()
	}
	defer unix.Close(info)
	data, _, err := readSecretFile(info, "exclude", 64<<10, os.Geteuid(), 0)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return secretFailure()
	}
	text := string(data)
	for _, file := range files {
		pattern := "/" + strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]", " ", "\\ ", "#", "\\#", "!", "\\!").Replace(file.Name)
		found := false
		for _, line := range strings.Split(text, "\n") {
			if line == pattern {
				found = true
				break
			}
		}
		if !found {
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			text += pattern + "\n"
		}
	}
	if len(text) > 64<<10 {
		return secretFailure()
	}
	if err := replaceSecretFile(info, "exclude", []byte(text), 0600, os.Geteuid(), os.Getegid(), false); err != nil {
		return err
	}
	for _, file := range files {
		// check-ignore alone is insufficient for a tracked but missing file.
		tracked, err := secretGit(root, "ls-files", "--cached", "-z", "--", ":(literal)"+file.Name)
		if err != nil || len(tracked) != 0 {
			return secretFailure()
		}
		if _, err := secretGit(root, "check-ignore", "--quiet", "--", file.Name); err != nil {
			return secretFailure()
		}
	}
	return nil
}

// secretGit runs read-only plumbing with a pinned working directory and no app
// environment, hooks, fsmonitor process, pager or user/system Git configuration.
// Only metadata paths enter argv; values and bundle variables never do.
func secretGit(root int, args ...string) ([]byte, error) {
	fd, err := unix.Dup(root)
	if err != nil {
		return nil, secretFailure()
	}
	file := os.NewFile(uintptr(fd), "project directory")
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixed := []string{"--git-dir=.git", "--work-tree=.", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.excludesFile=/dev/null"}
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append(fixed, args...)...)
	cmd.ExtraFiles = []*os.File{file}
	cmd.Dir = "/proc/self/fd/" + strconv.Itoa(fd)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1"}
	out, err := cmd.Output()
	if err != nil {
		return nil, secretFailure()
	}
	return out, nil
}
