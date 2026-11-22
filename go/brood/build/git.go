package build

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

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
