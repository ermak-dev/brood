package build

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// MaxSnapshotBytes counts the entire uncompressed TAR, including headers and padding.
	MaxSnapshotBytes = 64 << 20
	// MaxSnapshotFiles is the maximum number of regular files in a snapshot.
	MaxSnapshotFiles = 20000

	snapshotBlockSize = 512
)

// WriteSnapshot writes the current contents of tracked and non-ignored untracked
// files to an uncompressed USTAR archive. Missing files and symlinks (including
// paths with symlink parents) are skipped; submodules are not supported.
// The caller must discard the output if WriteSnapshot returns an error.
func (p *Project) WriteSnapshot(ctx context.Context, out io.Writer) error {
	if p.Root == "" {
		return errors.New("a working-copy snapshot requires running from a Git repository root")
	}
	top, err := snapshotGit(ctx, p.Root, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(strings.TrimSuffix(top, "\n"))
	if err != nil || actual != p.Root {
		return fmt.Errorf("%s must be at the Git repository root to build from the working copy", Filename)
	}
	root, err := os.OpenRoot(p.Root)
	if err != nil {
		return errors.New("could not open the Git repository root for the snapshot")
	}
	defer root.Close()

	excluded, secrets, err := p.snapshotSecrets(root)
	if err != nil {
		return err
	}
	// Gitlink проверяется по индексу: каталог подмодуля может отсутствовать.
	staged, err := snapshotGit(ctx, p.Root, "ls-files", "--cached", "--stage", "-z")
	if err != nil {
		return err
	}
	for entry := range strings.SplitSeq(staged, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			_, name, _ := strings.Cut(entry, "\t")
			if !snapshotExcluded(name, excluded) {
				return fmt.Errorf("cannot snapshot submodule %q; working-copy builds do not support submodules", name)
			}
		}
	}
	listed, err := snapshotGit(ctx, p.Root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}

	writer := tar.NewWriter(out)
	seen := make(map[string]struct{})
	remaining := int64(MaxSnapshotBytes - 2*snapshotBlockSize)
	for name := range strings.SplitSeq(listed, "\x00") {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate || name == "" || snapshotExcluded(name, excluded) {
			continue
		}
		if !ValidSnapshotPath(name) {
			return fmt.Errorf("cannot snapshot %q; expected a regular file with a relative archive path", name)
		}
		size, err := writeSnapshotFile(ctx, root, writer, name, secrets, remaining, len(seen))
		if err != nil {
			return fmt.Errorf("cannot snapshot %q: %w", name, err)
		}
		if size != 0 {
			seen[name] = struct{}{}
			remaining -= size
		}
	}
	if _, ok := seen[Filename]; !ok {
		return fmt.Errorf("working-copy snapshot requires a regular, non-excluded %s at the Git repository root", Filename)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("could not finish the working-copy snapshot: %w", err)
	}
	return nil
}

func snapshotGit(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	// Внешнее окружение не должно переносить сюда индекс или дерево другого репозитория.
	for _, env := range cmd.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch key {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_IMPLICIT_WORK_TREE", "GIT_PREFIX":
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	data, err := cmd.Output()
	if err == nil {
		return string(data), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "", errors.New("working-copy snapshots require Git; install Git and retry")
	}
	if args[0] == "rev-parse" {
		return "", errors.New("a working-copy snapshot requires a readable Git working tree; run from its root")
	}
	return "", errors.New("could not list Git working-copy files; check the repository and its index")
}

func snapshotExcluded(name string, secrets map[string]struct{}) bool {
	_, secret := secrets[name]
	return secret || ExcludedSnapshotPath(name)
}

// ValidSnapshotPath reports whether name is a portable relative archive path.
func ValidSnapshotPath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00\r\n") && !patternDrive.MatchString(name)
}

// ExcludedSnapshotPath reports whether name refers to Git metadata or a conventional secret path.
func ExcludedSnapshotPath(name string) bool {
	if name == "secrets" || strings.HasPrefix(name, "secrets/") {
		return true
	}
	for part := range strings.SplitSeq(strings.ToLower(name), "/") {
		if part == ".git" || part == ".env" || strings.HasPrefix(part, ".env.") {
			return true
		}
	}
	return false
}

// snapshotSecrets запоминает имена, цели симлинков и идентичность файлов,
// не читая значений. Идентичность нужна для исключения жёстких ссылок.
func (p *Project) snapshotSecrets(root *os.Root) (map[string]struct{}, []fs.FileInfo, error) {
	excluded := make(map[string]struct{})
	var identities []fs.FileInfo
	if p.Secrets == nil {
		return excluded, identities, nil
	}
	sources := make([]string, 0, len(p.Secrets.Files)+1)
	if p.Secrets.Env != "" {
		sources = append(sources, p.Secrets.Env)
	}
	for _, file := range p.Secrets.Files {
		sources = append(sources, file.Source)
	}
	for _, name := range sources {
		if _, duplicate := excluded[name]; duplicate {
			continue
		}
		excluded[name] = struct{}{}
		resolved, err := filepath.EvalSymlinks(filepath.Join(p.Root, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("cannot resolve secret source %q; check the working copy", name)
		}
		relative, err := filepath.Rel(p.Root, resolved)
		if err != nil || !filepath.IsLocal(relative) {
			return nil, nil, fmt.Errorf("secret source %q resolves outside the Git repository; use a source inside it", name)
		}
		excluded[filepath.ToSlash(relative)] = struct{}{}
		info, err := root.Stat(relative)
		if err != nil || !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("secret source %q must resolve to a readable regular file to snapshot safely", name)
		}
		identities = append(identities, info)
	}
	return excluded, identities, nil
}

// snapshotLstat не проходит по ссылкам, даже если в индексе их место занимает каталог.
// nil без ошибки означает, что путь исчез или содержит симлинк.
func snapshotLstat(root *os.Root, name string) (fs.FileInfo, error) {
	for i := 0; ; {
		slash := strings.IndexByte(name[i:], '/')
		end := len(name)
		if slash >= 0 {
			end = i + slash
		}
		info, err := root.Lstat(filepath.FromSlash(name[:end]))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		if err != nil {
			return nil, errors.New("could not inspect the working-copy path")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil
		}
		if slash < 0 {
			if !info.Mode().IsRegular() {
				return nil, errors.New("expected a regular file; directories and special files are unsupported")
			}
			return info, nil
		}
		if !info.IsDir() {
			return nil, nil
		}
		i = end + 1
	}
}

func writeSnapshotFile(ctx context.Context, root *os.Root, out *tar.Writer, name string, secrets []fs.FileInfo, remaining int64, count int) (int64, error) {
	before, err := snapshotLstat(root, name)
	if before == nil || err != nil {
		return 0, err
	}
	for _, secret := range secrets {
		if os.SameFile(secret, before) {
			return 0, nil
		}
	}
	// NONBLOCK не даёт зависнуть, если обычный файл подменили FIFO.
	f, err := root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, errors.New("could not safely open the file; check the working copy and retry")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return 0, errors.New("file changed while opening it; retry with a stable working copy")
	}
	// Повторная проверка ловит подмену компонентов между Lstat и OpenFile.
	after, err := snapshotLstat(root, name)
	if err != nil || after == nil || !os.SameFile(after, info) {
		return 0, errors.New("path changed while opening it; retry with a stable working copy")
	}
	if count >= MaxSnapshotFiles {
		return 0, fmt.Errorf("snapshot exceeds %d files; remove or ignore files and retry", MaxSnapshotFiles)
	}
	// Остаток кратен блоку, поэтому после заголовка округление размера в него помещается.
	if info.Size() < 0 || info.Size() > remaining-snapshotBlockSize {
		return 0, fmt.Errorf("snapshot exceeds %d bytes; remove or ignore files and retry", MaxSnapshotBytes)
	}
	blocks := 1 + (info.Size()+snapshotBlockSize-1)/snapshotBlockSize
	if err := out.WriteHeader(&tar.Header{
		Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
	}); err != nil {
		return 0, fmt.Errorf("could not write USTAR header: %w", err)
	}
	if _, err := io.CopyN(out, snapshotReader{ctx: ctx, in: f}, info.Size()); err != nil {
		return 0, fmt.Errorf("could not copy file to the snapshot: %w", err)
	}
	current, err := f.Stat()
	if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return 0, errors.New("file changed while reading it; retry with a stable working copy")
	}
	return blocks * snapshotBlockSize, nil
}

type snapshotReader struct {
	ctx context.Context
	in  io.Reader
}

func (r snapshotReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}
