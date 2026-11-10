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

// FEATURE: Broodfile reference to a server-only Git credential (#173).
// SPEC: project.git_credential names a project Git credential, never a value:
// a lowercase DNS label, or "" to clear the reference explicitly; absent keeps
// the server's reference. A reference with an explicitly empty repository is
// a contradiction. Refusals do not echo the field.
func TestProjectDeclarationGitCredential(t *testing.T) {
	plan := func(project map[string]any) (map[string]any, error) {
		doc := minimal(t)
		doc["project"] = project
		resolved, err := MakePlan(doc, nil, "", "localhost:5000/team", "development", "Broodfile")
		if err != nil {
			return nil, err
		}
		return resolved["projectConfig"].(map[string]any), nil
	}
	for _, reference := range []string{"gitlab", ""} {
		config, err := plan(map[string]any{"repository": "https://git.example.com/team/repo.git", "git_credential": reference})
		wantNoError(t, err)
		if value, present := config["git_credential"]; !present || value != reference {
			t.Fatalf("reference %q = %v", reference, config)
		}
	}
	config, err := plan(map[string]any{"git_credential": "gitlab"})
	wantNoError(t, err)
	if config["git_credential"] != "gitlab" {
		t.Fatalf("reference with resolved repository = %v", config)
	}
	config, err = plan(map[string]any{"repository": "https://git.example.com/team/repo.git"})
	wantNoError(t, err)
	if _, present := config["git_credential"]; present {
		t.Fatalf("absent reference became explicit: %v", config)
	}
	for _, project := range []map[string]any{
		{"git_credential": "DO_NOT_ECHO"},
		{"git_credential": "do_not_echo"},
		{"git_credential": "do-not-echo:token"},
		{"git_credential": strings.Repeat("a", 64)},
		{"git_credential": true},
		{"git_credential": nil},
		{"git_credential": map[string]any{"token": "DO_NOT_ECHO"}},
		{"repository": "", "git_credential": "gitlab"},
	} {
		if _, err := plan(project); err == nil || strings.Contains(err.Error(), "DO_NOT_ECHO") || strings.Contains(err.Error(), "do_not_echo") {
			t.Fatalf("invalid reference %v accepted or exposed: %v", project, err)
		}
	}
}
