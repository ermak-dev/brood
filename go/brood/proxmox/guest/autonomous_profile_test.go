//go:build linux

package guest

// FEATURE: Autonomous Claude Code profile of managed actors (#164).
// SPEC: Messages of other sessions are
// accepted without the Deliver/Deny dialog (crossSessionInbound "accept"); the
// built-in tools SendMessage, ListAgents, AskUserQuestion, EnterWorktree,
// ExitWorktree and SendFeedback are denied, which removes them from the tool
// list of the model; feedback drafts are off; the fixed environment disables
// the /feedback command and the quality survey. The session gets the
// profile merged into ~/.claude/settings.json: every other key of the actor and
// every permission entry it has survive. No context window is pinned without an installation
// setting, and nothing of the operator's own wrapper (TLS bypass) is carried.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/projectsecret"
)

// autonomousTestDeny is the profile deny list in its documented order.
var autonomousTestDeny = []string{"SendMessage", "ListAgents", "AskUserQuestion", "EnterWorktree", "ExitWorktree", "SendFeedback"}

func assertAutonomousProfile(t *testing.T, settings map[string]any) {
	t.Helper()
	if settings["crossSessionInbound"] != "accept" || settings["feedbackDrafts"] != "off" {
		t.Fatalf("autonomous profile missing: %v", settings)
	}
	permissions, _ := settings["permissions"].(map[string]any)
	deny, _ := permissions["deny"].([]any)
	for _, name := range autonomousTestDeny {
		count := 0
		for _, entry := range deny {
			if entry == name {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("deny holds %s %d times: %v", name, count, deny)
		}
	}
}

func canonicalJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestClaudeSettingsMergeKeepsActorEntries(t *testing.T) {
	home := privateTemp(t)
	os.Mkdir(filepath.Join(home, ".claude"), 0700)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{
  "model": "glm-5.3",
  "env": {"FOO": "bar"},
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/workspace/home/stop.sh"}]}]},
  "statusLine": {"type": "command", "command": "/workspace/home/line.sh"},
  "custom": {"n": 12345678901234567890, "f": 2.50},
  "crossSessionInbound": "hold",
  "feedbackDrafts": "notify",
  "permissions": {
    "allow": ["Bash(go test:*)"],
    "ask": ["Bash(git push:*)"],
    "deny": ["Bash(rm -rf:*)", "ListAgents", 7],
    "defaultMode": "acceptEdits",
    "additionalDirectories": ["/workspace/home/notes"]
  }
}`), 0600)
	if err := prepareClaudeConfig(home, claudeTestProject); err != nil {
		t.Fatal(err)
	}
	_, settings := assertUnattended(t, home)
	// Ключи профиля принадлежат brood: «hold» актора вернул бы диалог
	// Deliver/Deny, «notify» — черновики feedback.
	assertAutonomousProfile(t, settings)
	permissions := settings["permissions"].(map[string]any)
	want := `["Bash(rm -rf:*)","ListAgents",7,"SendMessage","AskUserQuestion","EnterWorktree","ExitWorktree","SendFeedback"]`
	if got := canonicalJSON(t, permissions["deny"]); got != want {
		t.Fatalf("deny %s, want %s", got, want)
	}
	for key, want := range map[string]string{"allow": `["Bash(go test:*)"]`, "ask": `["Bash(git push:*)"]`, "defaultMode": `"acceptEdits"`, "additionalDirectories": `["/workspace/home/notes"]`} {
		if got := canonicalJSON(t, permissions[key]); got != want {
			t.Errorf("permissions.%s %s, want %s", key, got, want)
		}
	}
	for key, want := range map[string]string{
		"model":      `"glm-5.3"`,
		"env":        `{"FOO":"bar"}`,
		"hooks":      `{"Stop":[{"hooks":[{"command":"/workspace/home/stop.sh","type":"command"}]}]}`,
		"statusLine": `{"command":"/workspace/home/line.sh","type":"command"}`,
		"custom":     `{"f":2.50,"n":12345678901234567890}`,
	} {
		if got := canonicalJSON(t, settings[key]); got != want {
			t.Errorf("%s %s, want %s", key, got, want)
		}
	}
}

// SPEC: A permissions value Claude Code could not read becomes the profile
// deny list; the members it can read stay.
func TestClaudeSettingsRepairMalformedPermissions(t *testing.T) {
	deny := `["SendMessage","ListAgents","AskUserQuestion","EnterWorktree","ExitWorktree","SendFeedback"]`
	for content, want := range map[string]string{
		`{"permissions":"all"}`:                                   `{"deny":` + deny + `}`,
		`{"permissions":null}`:                                    `{"deny":` + deny + `}`,
		`{"permissions":["Bash"]}`:                                `{"deny":` + deny + `}`,
		`{"permissions":{"allow":["Read"],"deny":"SendMessage"}}`: `{"allow":["Read"],"deny":` + deny + `}`,
		`{"permissions":{"ask":["Bash"],"deny":null}}`:            `{"ask":["Bash"],"deny":` + deny + `}`,
		`{"permissions":{"deny":["SendFeedback","SendMessage"]}}`: `{"deny":["SendFeedback","SendMessage","ListAgents","AskUserQuestion","EnterWorktree","ExitWorktree"]}`,
	} {
		t.Run(content, func(t *testing.T) {
			home := privateTemp(t)
			os.Mkdir(filepath.Join(home, ".claude"), 0700)
			os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(content), 0600)
			if err := prepareClaudeConfig(home, claudeTestProject); err != nil {
				t.Fatal(err)
			}
			_, settings := assertUnattended(t, home)
			if got := canonicalJSON(t, settings["permissions"]); got != want {
				t.Fatalf("permissions %s, want %s", got, want)
			}
		})
	}
}

// SPEC: The session environment disables /feedback and the quality
// survey and pins no context window without an installation setting, even
// with installation models; a project secret cannot switch feedback back on.
func TestClaudeLaunchDisablesFeedbackWithoutContextPin(t *testing.T) {
	layout := appLayout{Project: privateTemp(t), Home: privateTemp(t), Run: privateTemp(t), RunUID: os.Geteuid(), RunGID: os.Getegid()}
	if err := publishProjectSecrets(layout.Run, projectsecret.Bundle{Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}}, layout.RunUID, layout.RunGID); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkspaceOrigin(layout.Run, WorkspaceOrigin{Models: actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}}, layout.RunUID, layout.RunGID); err != nil {
		t.Fatal(err)
	}
	env, err := prepareProjectApp(layout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DISABLE_FEEDBACK_COMMAND=1", "CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1", "ANTHROPIC_MODEL=glm-5.3"} {
		if !slices.Contains(env, want) {
			t.Errorf("session env lacks %s: %q", want, env)
		}
	}
	for _, item := range env {
		if strings.HasPrefix(item, "CLAUDE_CODE_MAX_CONTEXT_TOKENS=") || strings.HasPrefix(item, "NODE_TLS_REJECT_UNAUTHORIZED=") {
			t.Errorf("session env carries %s", item)
		}
	}
	if _, err := projectAppEnvironment(projectsecret.Bundle{Vars: map[string]string{"DISABLE_FEEDBACK_COMMAND": "0"}}); err == nil {
		t.Fatal("project secret overrides the fixed feedback environment")
	}
}
