package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FEATURE: Декларация серверного проекта в Broodfile (#172).
// SPEC: Явный repository переопределяет origin текущего проекта; SSH origin
// нормализуется без credentials. Локальный plan не требует login или Git.
func TestProjectDeclarationExplicitRepository(t *testing.T) {
	root := workingFixture(t)
	file := filepath.Join(root, "Broodfile")
	data, err := os.ReadFile(file)
	wantNoError(t, err)
	wantNoError(t, os.WriteFile(file, append(data, []byte("project:\n  repository: https://Git.Example.com/team/other.git\n")...), 0600))
	loaded, err := LoadWorkingProject(root, "", "destination")
	wantNoError(t, err)
	config, _ := loaded.Plan["projectConfig"].(map[string]any)
	if config["name"] != "destination" || config["repository"] != "https://git.example.com/team/other.git" {
		t.Fatalf("project declaration = %v", config)
	}
}

func TestProjectDeclarationDefaultsToCurrentGitOrigin(t *testing.T) {
	for _, remote := range []string{"https://git.example.com/team/current.git", "git@git.example.com:team/current.git", "ssh://git@git.example.com/team/current.git"} {
		t.Run(remote, func(t *testing.T) {
			f := newProjectFixture(t)
			f.git("remote", "set-url", "origin", remote)
			loaded, err := LoadWorkingProject(f.repo, "", "")
			wantNoError(t, err)
			config, _ := loaded.Plan["projectConfig"].(map[string]any)
			if config["repository"] != "https://git.example.com/team/current.git" || config["name"] != "example" {
				t.Fatalf("resolved project = %v", config)
			}
		})
	}
}

func TestProjectDeclarationRejectsUnsafeRepositoryWithoutEcho(t *testing.T) {
	for _, remote := range []string{"https://user:DO_NOT_LOG_SECRET@git.example.com/team/repo.git", "https://git.example.com/team/repo.git?token=DO_NOT_LOG_SECRET", "file:///tmp/private", "https://127.0.0.1/team/repo.git", "ssh://git@git.example.com:2222/team/repo.git"} {
		root := workingFixture(t)
		file := filepath.Join(root, "Broodfile")
		data, err := os.ReadFile(file)
		wantNoError(t, err)
		wantNoError(t, os.WriteFile(file, append(data, []byte("project:\n  repository: "+remote+"\n")...), 0600))
		_, err = LoadWorkingProject(root, "", "")
		if err == nil || strings.Contains(err.Error(), "DO_NOT_LOG_SECRET") {
			t.Fatalf("unsafe project repository accepted or exposed: %v", err)
		}
	}
}

func TestProjectDeclarationOfflineWithoutGit(t *testing.T) {
	root := workingFixture(t)
	loaded, err := LoadWorkingProject(root, "", "")
	wantNoError(t, err)
	config, _ := loaded.Plan["projectConfig"].(map[string]any)
	if config["name"] != "example" || config["repository"] != "" {
		t.Fatalf("offline project = %v", config)
	}
}
