package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FEATURE: сборка читает текущий Broodfile, справка разделена по командам.
// SPEC: локальная сборка без login/registry/commit, project — имя, не путь.
func TestBuildHelpBelongsToBuild(t *testing.T) {
	code, out, diagnostic := invoke(t, "", "--help")
	if code != 0 || diagnostic != "" || !strings.Contains(out, "push") {
		t.Fatalf("root help: %d %q %q", code, out, diagnostic)
	}
	for _, option := range []string{"--project", "--ref", "--registry", "--atespace", "--export"} {
		if strings.Contains(out, option) {
			t.Fatalf("root help contains build option %s", option)
		}
	}
	code, out, diagnostic = invoke(t, "", "build", "--help")
	if code != 0 || diagnostic != "" || !strings.Contains(out, "--project") || !strings.Contains(out, "--plan") || strings.Contains(out, "--ref") || strings.Contains(out, "--atespace") || strings.Contains(out, "--registry") {
		t.Fatalf("build help: %d %q %q", code, out, diagnostic)
	}
}

func TestBuildPlanUsesCurrentDirectoryWithoutGit(t *testing.T) {
	root := gitProject(t)
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "Broodfile"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "name: example", "name: working-copy", 1))
	if err := os.WriteFile(filepath.Join(root, "Broodfile"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", "invalid-relative-state")
	code, out, diagnostic := invoke(t, "", "build", "development", "--project", "my-project", "--plan")
	var plan map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil || plan["project"] != "working-copy" || plan["atespace"] != "my-project" {
		t.Fatalf("current files: %d %q %q", code, out, diagnostic)
	}
}

func TestBuildRejectsRemovedGitSelectionOptions(t *testing.T) {
	for _, option := range []string{"--ref", "--atespace", "--registry"} {
		code, _, diagnostic := invoke(t, "", "build", option, "value")
		if code == 0 || !strings.Contains(diagnostic, "flag provided but not defined") {
			t.Fatalf("%s: %d %q", option, code, diagnostic)
		}
	}
}
