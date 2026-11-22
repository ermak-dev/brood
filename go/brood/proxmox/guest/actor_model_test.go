//go:build linux

package guest

// FEATURE: Installation Claude Code models in the actor environment (#263).
// SPEC: The workspace response may carry models {model, smallModel}, each an
// optional printable ASCII name, with or without origin. The supervisor stages
// them with the origin, and the app wrapper adds them to the Claude Code
// environment of pinned 2.1.310: the main model is the session model and the
// opus and sonnet aliases, the small model the haiku alias, which also serves
// background tasks. Without models the environment is the previous one, so
// an old server without the field keeps the old behavior.

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/projectsecret"
)

func TestClaudeModelEnv(t *testing.T) {
	for _, tc := range []struct {
		models actormodel.Models
		want   []string
	}{
		{actormodel.Models{}, nil},
		{actormodel.Models{Model: "glm-5.3"}, []string{"ANTHROPIC_MODEL=glm-5.3", "ANTHROPIC_DEFAULT_OPUS_MODEL=glm-5.3", "ANTHROPIC_DEFAULT_SONNET_MODEL=glm-5.3"}},
		{actormodel.Models{Small: "glm-5.3-flash"}, []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL=glm-5.3-flash"}},
		{actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}, []string{"ANTHROPIC_MODEL=glm-5.3", "ANTHROPIC_DEFAULT_OPUS_MODEL=glm-5.3", "ANTHROPIC_DEFAULT_SONNET_MODEL=glm-5.3", "ANTHROPIC_DEFAULT_HAIKU_MODEL=glm-5.3-flash"}},
	} {
		if got := claudeModelEnv(tc.models); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: %q; want %q", tc.models, got, tc.want)
		}
	}
	// Fable-пин делает основную модель «моделью Fable» для Claude Code (имя,
	// промпт, fallback), а модель подагентов по умолчанию и так наследует
	// основную: ни того, ни другого гость не задаёт.
	all := strings.Join(claudeModelEnv(actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}), "\n")
	for _, absent := range []string{"ANTHROPIC_DEFAULT_FABLE_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_MODEL="} {
		if strings.Contains(all, absent) {
			t.Errorf("%s set", absent)
		}
	}
	for _, item := range actorEnv() {
		if strings.HasPrefix(item, "ANTHROPIC_MODEL=") || strings.HasPrefix(item, "ANTHROPIC_DEFAULT_") {
			t.Fatalf("fixed actor environment pins a model: %s", item)
		}
	}
}

func TestWorkspaceOriginFetchModels(t *testing.T) {
	server := newWorkspaceOriginServer(t)
	cfg := server.config()
	both := actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}
	for body, want := range map[string]WorkspaceOrigin{
		`{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","models":{"model":"glm-5.3","smallModel":"glm-5.3-flash"}}`: {Origin: cloneTestOrigin, Models: both},
		`{"origin":"","sshPrivateKey":"","models":{"model":"glm-5.3","smallModel":"glm-5.3-flash"}}`:                        {Models: both},
		`{"models":{"smallModel":"glm-5.3-flash"},"origin":"","sshPrivateKey":""}`:                                          {Models: actormodel.Models{Small: "glm-5.3-flash"}},
		`{"origin":"","sshPrivateKey":"","models":{"model":"glm-5.3"}}`:                                                     {Models: actormodel.Models{Model: "glm-5.3"}},
	} {
		server.serve(body, http.StatusOK)
		if got, err := fetchWorkspace(context.Background(), cfg); err != nil || got.WorkspaceOrigin != want {
			t.Fatalf("fetch %s: %+v %v", body, got, err)
		}
	}
	for name, models := range map[string]string{
		"null":           `null`,
		"empty object":   `{}`,
		"string":         `"glm-5.3"`,
		"empty name":     `{"model":""}`,
		"null name":      `{"model":null}`,
		"number":         `{"model":5}`,
		"space":          `{"model":"private model"}`,
		"newline":        `{"smallModel":"private\nANTHROPIC_API_KEY=x"}`,
		"non-ASCII":      `{"model":"private-модель"}`,
		"too long":       `{"model":"private-` + strings.Repeat("m", actormodel.MaxName) + `"}`,
		"unknown field":  `{"model":"glm-5.3","token":"private-token"}`,
		"duplicate name": `{"model":"glm-5.3","model":"private-model"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server.serve(`{"origin":"","sshPrivateKey":"","models":`+models+`}`, http.StatusOK)
			got, err := fetchWorkspace(context.Background(), cfg)
			if err == nil || got != (workspacePayload{}) || strings.Contains(err.Error(), "private") {
				t.Fatalf("not safely rejected: %+v %v", got, err)
			}
		})
	}
	server.serve(`{"origin":"","sshPrivateKey":"","models":{"model":"glm-5.3"},"models":{"model":"glm-5.3"}}`, http.StatusOK)
	if got, err := fetchWorkspace(context.Background(), cfg); err == nil {
		t.Fatalf("models twice accepted: %+v", got)
	}
}

func TestWorkspaceOriginProjectionModels(t *testing.T) {
	dir := privateTemp(t)
	for _, want := range []WorkspaceOrigin{
		{Models: actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}},
		{Origin: cloneTestOrigin, GitIdentity: cloneTestIdentity, Models: actormodel.Models{Small: "glm-5.3-flash"}},
	} {
		if err := publishWorkspaceOrigin(dir, want, os.Geteuid(), os.Getegid()); err != nil {
			t.Fatal(err)
		}
		if got, err := readWorkspaceOrigin(dir, os.Geteuid(), os.Getegid()); err != nil || got != want {
			t.Fatalf("read back %+v: %+v %v", want, got, err)
		}
	}
	for _, invalid := range []actormodel.Models{{Model: "glm 5.3"}, {Small: "glm-5.3\n"}} {
		if err := publishWorkspaceOrigin(dir, WorkspaceOrigin{Models: invalid}, os.Geteuid(), os.Getegid()); err == nil {
			t.Errorf("invalid models published: %+v", invalid)
		}
	}
}

func TestProjectAppPassesInstallationModels(t *testing.T) {
	run, project, home := privateTemp(t), privateTemp(t), privateTemp(t)
	if err := publishProjectSecrets(run, projectsecret.Bundle{Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}}, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	layout := appLayout{Project: project, Home: home, Run: run, RunUID: os.Geteuid(), RunGID: os.Getegid(), CloneTimeout: time.Second}
	prepare := func(origin WorkspaceOrigin) []string {
		t.Helper()
		if err := publishWorkspaceOrigin(run, origin, os.Geteuid(), os.Getegid()); err != nil {
			t.Fatal(err)
		}
		var warnings bytes.Buffer
		env, err := prepareProjectApp(layout, &warnings)
		if err != nil || warnings.Len() != 0 {
			t.Fatalf("app preparation: %v %q", err, warnings.String())
		}
		return env
	}
	previous := prepare(WorkspaceOrigin{})
	if !reflect.DeepEqual(previous, append(actorEnv(), "SERVICE_KEY=synthetic-env")) {
		t.Fatalf("environment without models changed: %q", previous)
	}
	env := prepare(WorkspaceOrigin{Models: actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}})
	if !reflect.DeepEqual(env, append(slices.Clone(previous), claudeModelEnv(actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"})...)) {
		t.Fatalf("models not appended to the Claude environment: %q", env)
	}
	for _, want := range []string{"ANTHROPIC_MODEL=glm-5.3", "ANTHROPIC_DEFAULT_HAIKU_MODEL=glm-5.3-flash", "ANTHROPIC_BASE_URL=http://127.0.0.1:8317"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s", want)
		}
	}
}
