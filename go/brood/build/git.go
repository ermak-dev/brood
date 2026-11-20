package build

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var patternCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidCommit reports whether value is a full lowercase SHA-1 commit ID.
func ValidCommit(value string) bool { return patternCommit.MatchString(value) }

// git выполняет команду в root; failed=true означает ненулевой код возврата.
func git(root string, arguments ...string) (output string, failed bool, err error) {
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return "", false, errors.New("git is not installed")
		}
		return "", true, nil
	}
	return stdout.String(), false, nil
}

// origin не наследует origin родителя выбранного каталога: Git нужен только
// рабочей копии, корень которой совпадает с каталогом brood.yaml.
func origin(root string) (remote string, present bool, err error) {
	toplevel, failed, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil || failed {
		// Git is optional for offline planning.
		return "", false, nil
	}
	if actual, err := filepath.EvalSymlinks(strings.TrimSpace(toplevel)); err != nil || actual != root {
		return "", false, nil
	}
	output, failed, err := git(root, "remote", "get-url", "origin")
	if err != nil || failed {
		return "", false, err
	}
	return strings.TrimSpace(output), true, nil
}

// Head returns the full commit ID checked out in root.
func Head(root string) (string, error) {
	output, failed, err := git(root, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(output)
	if failed || !ValidCommit(commit) {
		return "", fmt.Errorf("could not resolve HEAD to a full commit in %s", root)
	}
	return commit, nil
}

// Dirty reports uncommitted changes in root; known is false when Git cannot
// tell, for example outside a checkout.
func Dirty(root string) (dirty, known bool) {
	output, failed, err := git(root, "status", "--porcelain")
	if err != nil || failed {
		return false, false
	}
	return strings.TrimSpace(output) != "", true
}

// Published reports whether a remote-tracking branch of the checkout in root
// contains commit; known is false when Git cannot tell, for example outside a
// checkout or for a commit the checkout does not have.
func Published(root, commit string) (published, known bool) {
	if !ValidCommit(commit) {
		return false, false
	}
	output, failed, err := git(root, "branch", "-r", "--contains", commit)
	if err != nil || failed {
		return false, false
	}
	return strings.TrimSpace(output) != "", true
}
