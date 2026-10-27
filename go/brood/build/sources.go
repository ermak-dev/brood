// Снимки Git без локальных настроек, секретов и изменений рабочего дерева.
// Порт brood/dev/build/sources.py (issue #35): та же семантика, те же сообщения.
package build

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// gitRun не отдаёт stderr Git: тот может содержать credentials из конфигурации.
// missing=true возвращает отсутствие результата вместо отказа.
func gitRun(root string, missing bool, arguments ...string) (string, bool, error) {
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			// Бинарник не нашёлся: понятный отказ вместо трассировки.
			return "", false, fmt.Errorf("git is not installed")
		}
		if missing {
			return "", true, nil
		}
		return "", false, fmt.Errorf("Git %s failed in the selected source", arguments[0])
	}
	if !utf8.Valid(stdout.Bytes()) {
		return "", false, fmt.Errorf("Git source contains invalid UTF-8 paths or text")
	}
	return stdout.String(), false, nil
}

// Git выполняет команду в репозитории и возвращает stdout.
func Git(root string, arguments ...string) (string, error) {
	output, _, err := gitRun(root, false, arguments...)
	return output, err
}

// Revision разрешает ref в полный SHA-1 commit.
func Revision(root, ref string) (string, error) {
	output, _, err := gitRun(root, false, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(output)
	if !patternHex40.MatchString(value) {
		return "", fmt.Errorf("Source must resolve to a full SHA-1 Git commit")
	}
	return value, nil
}

// Origin возвращает проверенный URL origin; present=false, когда remote нет.
func Origin(root string, required bool) (value string, present bool, err error) {
	output, missing, err := gitRun(root, true, "remote", "get-url", "origin")
	if err != nil {
		return "", false, err
	}
	if missing {
		if required {
			return "", false, fmt.Errorf("Git metadata requires a safe origin URL")
		}
		return "", false, nil
	}
	remote, err := Repository(strings.TrimSpace(output))
	if err != nil {
		return "", false, err
	}
	return remote, true, nil
}

// checkTree отвергает секретные файлы в корне commit и управляющие символы.
func checkTree(root, sha string) error {
	output, _, err := gitRun(root, false, "ls-tree", "-rz", "--name-only", sha)
	if err != nil {
		return err
	}
	for _, path := range strings.Split(output, "\x00") {
		if path == "" {
			continue
		}
		first := strings.SplitN(path, "/", 2)[0]
		switch first {
		case ".env", "secrets", ".ate-dev-env.sh", ".brood":
			return fmt.Errorf("Source commit contains secret settings or generated build output")
		}
		for _, char := range path {
			if char < 32 || char == 127 {
				return fmt.Errorf("Source commit contains control characters in paths")
			}
		}
	}
	return nil
}

// resolveLoose разрешает ссылки как Path.resolve() в Python: нестрого,
// последний компонент может не существовать, обрыв не отказывает.
func resolveLoose(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(absolute)
	resolvedParent, err := resolvePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(absolute)), nil
}

// resolvePath доводит путь по ссылкам как os.path.realpath: обрыв ссылки
// останавливает разрешение, а не отказывает.
func resolvePath(path string) (string, error) {
	current := path
	for range 40 {
		target, err := os.Readlink(current)
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.Clean(current), nil
			}
			var pathError *fs.PathError
			if errors.As(err, &pathError) && errors.Is(pathError.Err, syscall.EINVAL) {
				// Не ссылка, а обычный путь: разрешение закончено.
				return filepath.Clean(current), nil
			}
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = target
	}
	return "", fmt.Errorf("Source symlink escapes its snapshot")
}

// inside проверяет, что path лежит строго под base.
func inside(base, path string) bool {
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// checkLinks отвергает символические ссылки, выводящие за пределы снимка.
func checkLinks(root string) error {
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == base {
			return nil
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		top := strings.SplitN(relative, string(filepath.Separator), 2)[0]
		if entry.IsDir() && top == ".git" {
			return fs.SkipDir
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			resolved, err := resolvePath(path)
			if err != nil {
				return err
			}
			if !inside(base, resolved) {
				return fmt.Errorf("Source symlink escapes its snapshot")
			}
		}
		return nil
	})
}

// Snapshot создаёт новый снимок точного commit, не копируя Git-конфигурацию клона.
func Snapshot(source, ref, target string, gitMetadata bool) (map[string]any, error) {
	resolved, err := resolveLoose(source)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, fmt.Errorf("Source snapshot destination already exists")
	}
	sha, err := Revision(resolved, ref)
	if err != nil {
		return nil, err
	}
	remote, remotePresent, err := Origin(resolved, gitMetadata)
	if err != nil {
		return nil, err
	}
	if err := checkTree(resolved, sha); err != nil {
		return nil, err
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(parent, ".snapshot-")
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(staging)
		}
	}()
	if _, _, err := gitRun(staging, false, "init", "-q", "--template="); err != nil {
		return nil, err
	}
	if _, _, err := gitRun(staging, false, "-c", "core.hooksPath=/dev/null", "fetch", "-q",
		"--depth=1", "--update-shallow", "--no-tags", "--", resolved, sha); err != nil {
		return nil, err
	}
	if _, _, err := gitRun(staging, false, "-c", "core.hooksPath=/dev/null",
		"-c", "core.autocrlf=false", "-c", "core.logAllRefUpdates=false",
		"checkout", "-q", "--detach", sha); err != nil {
		return nil, err
	}
	if err := checkLinks(staging); err != nil {
		return nil, err
	}
	if gitMetadata {
		// Stat-поля index не должны превращать один commit в разные OCI-входы.
		if err := os.Remove(filepath.Join(staging, ".git", "index")); err != nil {
			return nil, err
		}
		if _, _, err := gitRun(staging, false, "read-tree", "--reset", sha); err != nil {
			return nil, err
		}
		if _, _, err := gitRun(staging, false, "remote", "add", "origin", remote); err != nil {
			return nil, err
		}
		if err := os.Remove(filepath.Join(staging, ".git", "FETCH_HEAD")); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if err := os.RemoveAll(filepath.Join(staging, ".git")); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(staging, target); err != nil {
		return nil, err
	}
	committed = true
	report := map[string]any{"revision": sha, "gitMetadata": gitMetadata}
	if remotePresent {
		report["repository"] = remote
	}
	return report, nil
}
