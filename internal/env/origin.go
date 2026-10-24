// Координаты трекера: сайт и проект берутся из origin рабочей копии, а не
// из настройки. Проект в трекере и проект в git — один проект; второе место
// правды однажды разойдётся с первым. .env переопределяет — когда трекер
// живёт не там, где код.
package env

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// parseOrigin разбирает url origin: scp, ssh:// с портом, https. Порт и
// схема отбрасываются — они не часть пути проекта.
var scpLike = regexp.MustCompile(`^(?:ssh://)?(?:[^@]+@)?([^:/]+)(?::(\d+))?(?::|/)(.+?)(?:\.git)?$`)

// ParseOrigin -> (host, project path); пусто, если origin не похож на url.
func ParseOrigin(url string) (host, project string) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", ""
	}
	m := scpLike.FindStringSubmatch(url)
	if m == nil {
		return "", ""
	}
	path := strings.TrimSuffix(m[3], ".git")
	if m[1] == "" || path == "" {
		return "", ""
	}
	return m[1], path
}

// Origin — url origin рабочей копии; пусто, если это не копия.
func Origin() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Site — хост GitLab.
func Site() (string, error) {
	if s := Get("GITLAB_SITE"); s != "" {
		return s, nil
	}
	if host, _ := ParseOrigin(Origin()); host != "" {
		return host, nil
	}
	return "", fmt.Errorf("no GitLab site: set GITLAB_SITE in .env or add a git origin")
}

// Project — путь проекта GitLab (group/name).
func Project() (string, error) {
	if p := Get("GITLAB_PROJECT"); p != "" {
		return p, nil
	}
	if _, p := ParseOrigin(Origin()); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no GitLab project: set GITLAB_PROJECT in .env or add a git origin")
}
