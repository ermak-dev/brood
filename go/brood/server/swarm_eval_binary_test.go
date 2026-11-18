package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/proxmox"
)

// FEATURE: Собранный bro проходит двухэтапный eval через настоящий Go API.
// SPEC: Без mock HTTP responses, production credentials и model/provider effects;
// pinned source/goal/model, boolean replay, refused restart и сравнение сохранены.
func TestSwarmEvalCompiledCLIThroughGoAPI(t *testing.T) {
	f := evalAPISetup(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "bro")
	build := exec.Command("go", "build", "-o", binary, "./cmd/bro")
	build.Dir = filepath.Clean(filepath.Join(cwd, "..", ".."))
	// Исходники CLI читаются явно: subprocess-сборка не входит в test-cache dependency graph.
	cliSources := filepath.Join(build.Dir, "cmd", "bro")
	entries, err := os.ReadDir(cliSources)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			if _, err := os.ReadFile(filepath.Join(cliSources, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, data)
	}
	state := t.TempDir()
	sessions, err := clientapi.NewSessionStore(filepath.Join(state, "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse(f.ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	for _, cookie := range f.alice.Jar.Cookies(origin) {
		if cookie.Name == auth.CookieSession {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatal("fixture operator has no authenticated session")
	}
	if err := sessions.Save(origin.String(), clientapi.StoredSession{Session: token, CSRF: f.ts.csrf(f.alice)}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetCurrent(origin.String()); err != nil {
		t.Fatal(err)
	}
	var providerCalls atomic.Int32
	f.ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		providerCalls.Add(1)
		t.Error("human-only eval reached a physical provider")
		return nil, context.Canceled
	}
	run := func(wantSuccess bool, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + state, "XDG_STATE_HOME=" + state, "LANG=C.UTF-8"}
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		err := cmd.Run()
		if (err == nil) != wantSuccess {
			t.Fatalf("compiled CLI %v: success=%t error=%v diagnostic=%s", args, wantSuccess, err, diagnostic.Bytes())
		}
		if !wantSuccess && (!strings.Contains(diagnostic.String(), "status 409") || !strings.Contains(diagnostic.String(), "swarm eval conflict")) {
			t.Fatalf("CLI refused before reaching the server's eval fence: %v: %s", args, diagnostic.Bytes())
		}
		return out.Bytes()
	}
	var bound struct {
		NonProduction bool                  `json:"nonProduction"`
		Source        clientapi.SwarmSource `json:"source"`
		Goal          string                `json:"goal"`
		Model         string                `json:"model"`
	}
	data := run(true, "swarm", "eval", "bind", f.candidate, "--reference", f.reference, "--pass", "1", "--definition", "approve", "--model", "selected-model-1", "--json")
	if err := json.Unmarshal(data, &bound); err != nil || !bound.NonProduction || bound.Source.Commit != startAPICommit || bound.Goal != startAPIGoal || bound.Model != "selected-model-1" {
		t.Fatalf("compiled CLI did not retain exact server selection: %s %v", data, err)
	}
	run(true, "swarm", "eval", "start", f.candidate, "--json")
	f.ts.srv.steps.wait()
	var status struct {
		Execution string `json:"execution"`
		Replay    *struct {
			State               string `json:"state"`
			InterruptedChildren bool   `json:"interruptedChildren"`
		} `json:"replay"`
	}
	data = run(true, "swarm", "eval", "status", f.candidate, "--json")
	if err := json.Unmarshal(data, &status); err != nil || status.Execution != "succeeded" || status.Replay == nil || status.Replay.State != "applied" || status.Replay.InterruptedChildren {
		t.Fatalf("compiled CLI could not read actual boolean replay status: %s %v", data, err)
	}
	run(false, "swarm", "eval", "start", f.candidate)
	run(false, "swarm", "start", f.candidate, "--definition", "approve", "--goal", "Different task")
	run(true, "swarm", "compare", f.reference, "1", f.candidate, "1", "--json")
	if providerCalls.Load() != 0 {
		t.Fatal("compiled CLI eval caused physical effects")
	}
}
