//go:build linux

package guest

// FEATURE: Unattended Claude start (#148).
// SPEC: Claude starts with --dangerously-skip-permissions and the brood MCP
// config. Before exec the UID 1000 wrapper merges onboarding, theme, resume and
// project trust into ~/.claude.json and skipDangerousModePermissionPrompt into
// ~/.claude/settings.json, preserving unknown keys. Writes are atomic, never
// through a symlink or hard link, limited to 1 MiB; a malformed file becomes a
// minimal valid object, never an empty file (mop edit_json lesson).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/projectsecret"
)

const claudeTestProject = "/workspace/project"

func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		t.Fatalf("%s is not a JSON object: %q %v", path, data, err)
	}
	return object
}

func assertUnattended(t *testing.T, home string) (map[string]any, map[string]any) {
	t.Helper()
	config := readJSONObject(t, filepath.Join(home, ".claude.json"))
	if config["hasCompletedOnboarding"] != true || config["resumeReturnDismissed"] != true {
		t.Fatalf("onboarding or resume dialog left enabled: %v", config)
	}
	projects, _ := config["projects"].(map[string]any)
	project, _ := projects[claudeTestProject].(map[string]any)
	if project["hasTrustDialogAccepted"] != true {
		t.Fatalf("project trust dialog left enabled: %v", config["projects"])
	}
	settings := readJSONObject(t, filepath.Join(home, ".claude", "settings.json"))
	if settings["skipDangerousModePermissionPrompt"] != true {
		t.Fatalf("bypass prompt left enabled: %v", settings)
	}
	for _, path := range []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json")} {
		if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
			t.Fatalf("%s mode: %v %v", path, info, err)
		}
	}
	return config, settings
}

func TestClaudeConfigFreshHome(t *testing.T) {
	home := privateTemp(t)
	if err := prepareClaudeConfig(home, claudeTestProject); err != nil {
		t.Fatal(err)
	}
	config, _ := assertUnattended(t, home)
	if config["theme"] != "dark" {
		t.Fatalf("default theme: %v", config["theme"])
	}
	if info, err := os.Lstat(filepath.Join(home, ".claude")); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("settings directory: %v %v", info, err)
	}
}

func TestClaudeConfigPreservesUserKeys(t *testing.T) {
	home := privateTemp(t)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{
  "userID": "user-1",
  "numStartups": 12345678901234567890,
  "theme": "light",
  "custom": {"nested": [1, 2.50, "x"]},
  "projects": {
    "/other": {"allowedTools": ["Bash"]},
    "/workspace/project": {"history": [{"display": "keep"}], "hasTrustDialogAccepted": false}
  }
}`), 0600)
	os.Mkdir(filepath.Join(home, ".claude"), 0700)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"model":"opus","permissions":{"allow":["Bash(go test:*)"]}}`), 0600)
	if err := prepareClaudeConfig(home, claudeTestProject); err != nil {
		t.Fatal(err)
	}
	config, settings := assertUnattended(t, home)
	if config["userID"] != "user-1" || config["numStartups"] != json.Number("12345678901234567890") || config["theme"] != "light" {
		t.Fatalf("user keys changed: %v", config)
	}
	if custom, _ := json.Marshal(config["custom"]); string(custom) != `{"nested":[1,2.50,"x"]}` {
		t.Fatalf("nested value changed: %s", custom)
	}
	projects := config["projects"].(map[string]any)
	if other, _ := json.Marshal(projects["/other"]); string(other) != `{"allowedTools":["Bash"]}` {
		t.Fatalf("other project changed: %s", other)
	}
	if history, _ := json.Marshal(projects[claudeTestProject].(map[string]any)["history"]); string(history) != `[{"display":"keep"}]` {
		t.Fatalf("project history changed: %s", history)
	}
	if settings["model"] != "opus" || settings["permissions"] == nil {
		t.Fatalf("user settings changed: %v", settings)
	}
}

func TestClaudeConfigReplacesMalformedWithMinimalObject(t *testing.T) {
	for _, content := range []string{"", "   \n", "{", "[]", "null", `"text"`, "42", `{"a":1} {"b":2}`, `{"projects":[]}`, `{"projects":{"/workspace/project":"text"}}`, `{"theme":null}`} {
		t.Run(content, func(t *testing.T) {
			home := privateTemp(t)
			os.Mkdir(filepath.Join(home, ".claude"), 0700)
			for _, path := range []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json")} {
				os.WriteFile(path, []byte(content), 0600)
			}
			if err := prepareClaudeConfig(home, claudeTestProject); err != nil {
				t.Fatal(err)
			}
			config, _ := assertUnattended(t, home)
			if config["theme"] != "dark" {
				t.Fatalf("theme after malformed input: %v", config["theme"])
			}
		})
	}
}

func TestClaudeConfigRefusesLinksAndOversize(t *testing.T) {
	outside := privateTemp(t)
	target := filepath.Join(outside, "target.json")
	cases := map[string]struct {
		refused string
		setup   func(home string)
	}{
		"symlink":  {".claude.json", func(home string) { os.Symlink(target, filepath.Join(home, ".claude.json")) }},
		"hardlink": {".claude.json", func(home string) { os.Link(target, filepath.Join(home, ".claude.json")) }},
		"oversize": {".claude.json", func(home string) {
			os.WriteFile(filepath.Join(home, ".claude.json"), append([]byte(`{"pad":"`), append(bytes.Repeat([]byte("x"), 1<<20), '"', '}')...), 0600)
		}},
		"settings directory symlink": {".claude", func(home string) { os.Symlink(outside, filepath.Join(home, ".claude")) }},
		"settings symlink": {".claude/settings.json", func(home string) {
			os.Mkdir(filepath.Join(home, ".claude"), 0700)
			os.Symlink(target, filepath.Join(home, ".claude", "settings.json"))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			os.WriteFile(target, []byte(`{"owner":"outside"}`), 0600)
			home := privateTemp(t)
			tc.setup(home)
			refused := filepath.Join(home, tc.refused)
			before, _ := os.Lstat(refused)
			content, _ := os.ReadFile(refused)
			if err := prepareClaudeConfig(home, claudeTestProject); err == nil || !strings.Contains(err.Error(), filepath.Base(tc.refused)) {
				t.Fatalf("unsafe Claude configuration accepted or unnamed: %v", err)
			}
			if data, _ := os.ReadFile(target); string(data) != `{"owner":"outside"}` {
				t.Fatalf("write through a link: %q", data)
			}
			if _, err := os.Stat(filepath.Join(outside, "settings.json")); !os.IsNotExist(err) {
				t.Fatal("settings written through a directory symlink")
			}
			after, _ := os.Lstat(refused)
			if data, _ := os.ReadFile(refused); !os.SameFile(before, after) || !bytes.Equal(content, data) {
				t.Fatal("refused file was replaced")
			}
		})
	}
}

func TestClaudeConfigIdempotent(t *testing.T) {
	home := privateTemp(t)
	if err := prepareClaudeConfig(home, claudeTestProject); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json")}
	var infos []os.FileInfo
	var contents [][]byte
	for _, path := range paths {
		info, _ := os.Lstat(path)
		data, _ := os.ReadFile(path)
		infos, contents = append(infos, info), append(contents, data)
	}
	time.Sleep(10 * time.Millisecond)
	if err := prepareClaudeConfig(home, claudeTestProject); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		info, _ := os.Lstat(path)
		data, _ := os.ReadFile(path)
		if !os.SameFile(infos[i], info) || !info.ModTime().Equal(infos[i].ModTime()) || !bytes.Equal(data, contents[i]) {
			t.Fatalf("%s rewritten on an unchanged start", path)
		}
	}
}

func TestClaudeStartsInBypassModeWithBroodMCP(t *testing.T) {
	args := projectAppArgs()
	if args[0] != "claude" || !slices.Contains(args, "--dangerously-skip-permissions") || slices.Contains(args, "--permission-mode") {
		t.Fatalf("Claude argv: %q", args)
	}
	at := slices.Index(args, "--mcp-config")
	if at < 0 || at+1 >= len(args) || !strings.Contains(args[at+1], `"brood"`) {
		t.Fatalf("brood MCP config lost: %q", args)
	}
}

func TestProjectAppPreparesUnattendedClaude(t *testing.T) {
	run, project, home := privateTemp(t), privateTemp(t), privateTemp(t)
	if err := publishProjectSecrets(run, projectsecret.Bundle{}, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkspaceOrigin(run, "", "", os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	layout := appLayout{Project: project, Home: home, Run: run, RunUID: os.Geteuid(), RunGID: os.Getegid(), Gateway: deadGateway(t), CloneTimeout: time.Second}
	if _, err := prepareProjectApp(layout, &warnings); err != nil || warnings.Len() != 0 {
		t.Fatalf("app preparation: %v %q", err, warnings.String())
	}
	config := readJSONObject(t, filepath.Join(home, ".claude.json"))
	projects, _ := config["projects"].(map[string]any)
	if trust, _ := projects[project].(map[string]any); trust["hasTrustDialogAccepted"] != true {
		t.Fatalf("app workspace not trusted: %v", config)
	}
	os.Remove(filepath.Join(home, ".claude.json"))
	os.Symlink(filepath.Join(t.TempDir(), "target"), filepath.Join(home, ".claude.json"))
	warnings.Reset()
	if _, err := prepareProjectApp(layout, &warnings); err != nil || !strings.Contains(warnings.String(), ".claude.json") {
		t.Fatalf("unsafe Claude configuration must warn, not block the app: %v %q", err, warnings.String())
	}
}
