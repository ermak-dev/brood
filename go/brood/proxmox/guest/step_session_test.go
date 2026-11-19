//go:build linux

package guest

// FEATURE: Step session policy (#224).
// SPEC: After a successful workspace_attest root starts the next Claude
// session in the step mode: Claude Code managed settings allow only managed
// hooks and carry the autonomous profile (#164), user and local settings are
// not loaded, MCP is brood alone, the Git global configuration is a root file
// with the identity only, and the bash startup files of the actor home are put
// aside in the quiet window, as in the image, which has none. No global Claude
// configuration of the home brings env into the step session. A free session
// (operator restart, CT start) keeps the launch it had before #224.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/proxmox/guest/managedinbox"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

var stepTestTime = time.Date(2026, 11, 11, 16, 45, 0, 0, time.UTC)

const stepTestStamp = "20261111T164500Z"

// stepTestPolicy places the root paths in a private tree: the test user
// stands in for root and the actor at once.
func stepTestPolicy(t *testing.T) stepPolicy {
	t.Helper()
	root := privateTemp(t)
	for _, dir := range []string{"run", "home", "etc"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return stepPolicy{Run: filepath.Join(root, "run"), Managed: filepath.Join(root, "etc", "claude-code"), Home: filepath.Join(root, "home"),
		Root: os.Geteuid(), RootGID: os.Getegid(), ActorGID: os.Getegid(), ActorUID: os.Geteuid(), Now: func() time.Time { return stepTestTime }}
}

func (p stepPolicy) dropIn() string {
	return filepath.Join(p.Managed, "managed-settings.d", stepManagedFile)
}

func assertOwnedMode(t *testing.T, path string, mode os.FileMode, uid, gid int) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if info.Mode() != mode || st.Uid != uint32(uid) || (gid >= 0 && st.Gid != uint32(gid)) || (!info.IsDir() && st.Nlink != 1) {
		t.Fatalf("%s: mode %v uid %d gid %d nlink %d, want %v uid %d gid %d", path, info.Mode(), st.Uid, st.Gid, st.Nlink, mode, uid, gid)
	}
}

func TestStepPolicyManagedSettingsAreRootOnly(t *testing.T) {
	p := stepTestPolicy(t)
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.dropIn())
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("managed settings are not a JSON object: %q", data)
	}
	// Только управляемые hooks; согласие на bypass и автономный профиль #164
	// переезжают сюда, потому что ~/.claude/settings.json в сессии шага не
	// читается. Ни hooks, ни env, ни MCP, ни plugins, ни statusLine здесь нет.
	want := map[string]any{"allowManagedHooksOnly": true, "skipDangerousModePermissionPrompt": true,
		"crossSessionInbound": "accept", "feedbackDrafts": "off",
		"permissions": map[string]any{"deny": []any{"SendMessage", "ListAgents", "AskUserQuestion", "EnterWorktree", "ExitWorktree", "SendFeedback"}}}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("managed settings %v, want %v", settings, want)
	}
	// Владелец root, запись только ему: UID 1000 не правит, не подменяет и не
	// удаляет политику. Claude читает её как other.
	assertOwnedMode(t, p.Managed, os.ModeDir|0755, p.Root, -1)
	assertOwnedMode(t, filepath.Dir(p.dropIn()), os.ModeDir|0755, p.Root, -1)
	assertOwnedMode(t, p.dropIn(), 0644, p.Root, p.RootGID)
	for _, name := range []string{stepSessionFile, stepGitConfigFile} {
		assertOwnedMode(t, filepath.Join(p.Run, name), 0440, p.Root, p.ActorGID)
	}
	for i := 0; i < 2; i++ {
		if err := p.leave(); err != nil {
			t.Fatalf("leave %d: %v", i, err)
		}
		for _, path := range []string{p.dropIn(), filepath.Join(p.Run, stepSessionFile), filepath.Join(p.Run, stepGitConfigFile)} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("%s left after leave: %v", path, err)
			}
		}
	}
	// Свободная сессия на CT без каталога политики: снимать нечего.
	if err := stepTestPolicy(t).leave(); err != nil {
		t.Fatal(err)
	}
}

func TestStepPolicyManagedInboxIsOptInAndRestorable(t *testing.T) {
	p := stepTestPolicy(t)
	layout := stepTestLayout(t, p)
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	baseline := readTestFile(t, p.dropIn())
	want := readJSONObject(t, p.dropIn())
	for _, key := range []string{"strictKnownMarketplaces", "extraKnownMarketplaces", "enabledPlugins"} {
		if _, present := want[key]; present {
			t.Fatalf("ordinary step drop-in has %s without inbox opt-in", key)
		}
	}
	bundle, ok := managedinbox.Package(true)
	if !ok {
		t.Fatal("managed inbox package unavailable")
	}
	assertStepLaunch := func() {
		t.Helper()
		args, _, err := prepareClaudeLaunch(layout, io.Discard)
		if err != nil || !slices.Equal(args, []string{
			"claude", "--dangerously-skip-permissions", "--setting-sources", "project", "--strict-mcp-config", "--mcp-config",
			`{"mcpServers":{"brood":{"command":"/opt/brood/bin/bro","args":["--guest","mcp"]}}}`,
		}) {
			t.Fatalf("step launch changed: %q, %v", args, err)
		}
	}

	if err := p.writeManagedInbox(bundle.ManagedSettings); err != nil {
		t.Fatal(err)
	}
	source := map[string]any{"source": "directory", "path": "/opt/brood/managed-marketplaces/supervisor-inbox"}
	want["strictKnownMarketplaces"] = []any{source}
	want["extraKnownMarketplaces"] = map[string]any{
		"brood-managed-inbox": map[string]any{"source": source, "autoUpdate": false},
	}
	want["enabledPlugins"] = map[string]any{"supervisor-inbox@brood-managed-inbox": true}
	if got := readJSONObject(t, p.dropIn()); !reflect.DeepEqual(got, want) {
		t.Fatalf("managed inbox drop-in %v, want %v", got, want)
	}
	assertOwnedMode(t, p.dropIn(), 0644, p.Root, p.RootGID)
	assertStepLaunch()

	ordinary := stepTestPolicy(t)
	if err := ordinary.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, ordinary.dropIn()); got != baseline {
		t.Fatalf("inbox opt-in changed the default step policy: %s", got)
	}

	if err := p.writeManaged(); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, p.dropIn()); got != baseline {
		t.Fatalf("ordinary step drop-in not restored: %s", got)
	}
	assertStepLaunch()

	for _, tc := range []struct{ key, value string }{
		{"mcpServers", `{}`},
		{"allowManagedHooksOnly", `false`},
		{"enabledPlugins", `{"supervisor-inbox@brood-managed-inbox":"true"}`},
		{"extraKnownMarketplaces", `{"brood-managed-inbox":{"source":{"source":"directory","path":"/opt/brood/elsewhere"}}}`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			var fragment map[string]json.RawMessage
			if err := json.Unmarshal(bundle.ManagedSettings, &fragment); err != nil {
				t.Fatal(err)
			}
			fragment[tc.key] = json.RawMessage(tc.value)
			data, err := json.Marshal(fragment)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.writeManagedInbox(data); err == nil {
				t.Fatalf("accepted unexpected %s value %s", tc.key, tc.value)
			}
			if got := readTestFile(t, p.dropIn()); got != baseline {
				t.Fatalf("rejected fragment changed the drop-in: %s", got)
			}
		})
	}
}

func TestStepPolicyRefusesManagedDirectoryActorCouldWrite(t *testing.T) {
	outside := privateTemp(t)
	for name, setup := range map[string]func(p stepPolicy){
		"writable parent": func(p stepPolicy) { os.Chmod(filepath.Dir(p.Managed), 0777) },
		"writable drop-in dir": func(p stepPolicy) {
			os.MkdirAll(filepath.Dir(p.dropIn()), 0700)
			os.Chmod(filepath.Dir(p.dropIn()), 0775)
		},
		"symlinked managed dir": func(p stepPolicy) { os.Symlink(outside, p.Managed) },
		"symlinked drop-in dir": func(p stepPolicy) { os.Mkdir(p.Managed, 0755); os.Symlink(outside, filepath.Dir(p.dropIn())) },
	} {
		t.Run(name, func(t *testing.T) {
			p := stepTestPolicy(t)
			setup(p)
			if err := p.enter(attestIdentity); err == nil {
				t.Fatal("step policy published into a directory the actor could change")
			}
			if _, err := os.Lstat(filepath.Join(p.Run, stepSessionFile)); !os.IsNotExist(err) {
				t.Fatalf("step marker left after refusal: %v", err)
			}
			if names := entries(t, outside); len(names) != 0 {
				t.Fatalf("written through a link: %v", names)
			}
		})
	}
}

// SPEC: The marker is written last and removed first, and nothing else is
// removed while it stays: the wrapper starts a step session on the marker
// alone, so a marker without the managed drop-in would be a step session
// without allowManagedHooksOnly. place is checked without the cleanup of
// enter, which a crash between its steps would not run.
func TestStepPolicyMarkerNeverOutlivesThePolicy(t *testing.T) {
	marker := func(p stepPolicy) string { return filepath.Join(p.Run, stepSessionFile) }
	for name, fail := range map[string]func(p *stepPolicy){
		"foreign home":          func(p *stepPolicy) { p.ActorUID = p.Root + 1 },
		"unsafe managed parent": func(p *stepPolicy) { os.Chmod(filepath.Dir(p.Managed), 0777) },
	} {
		t.Run("place "+name, func(t *testing.T) {
			p := stepTestPolicy(t)
			fail(&p)
			if err := p.place(attestIdentity); err == nil {
				t.Fatal("step policy placed despite the failure")
			}
			if _, err := os.Lstat(marker(p)); !os.IsNotExist(err) {
				t.Fatalf("step marker written before the policy it announces: %v", err)
			}
		})
	}
	t.Run("leave with the marker stuck", func(t *testing.T) {
		p := stepTestPolicy(t)
		if err := p.enter(attestIdentity); err != nil {
			t.Fatal(err)
		}
		// Каталог /run/brood, который пишет группа, — отказ снять отметку.
		if err := os.Chmod(p.Run, 0770); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(p.Run, 0700) })
		if err := p.leave(); err == nil || !strings.Contains(err.Error(), "marker") {
			t.Fatalf("leave with a stuck marker: %v", err)
		}
		for _, path := range []string{marker(p), p.dropIn(), filepath.Join(p.Run, stepGitConfigFile)} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("%s removed while the step marker stays: %v", path, err)
			}
		}
	})
}

// SPEC: The wrapper trusts only a marker of the root owner, the actor group,
// mode 0440 and the exact content; the group is checked, not just the owner.
func TestReadStepSessionChecksMarkerGroup(t *testing.T) {
	p := stepTestPolicy(t)
	if step, err := readStepSession(p.Run, p.Root, p.ActorGID); step || err != nil {
		t.Fatalf("no marker: %v %v", step, err)
	}
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	if step, err := readStepSession(p.Run, p.Root, p.ActorGID); !step || err != nil {
		t.Fatalf("root marker: %v %v", step, err)
	}
	if step, err := readStepSession(p.Run, p.Root, p.ActorGID+1); step || err == nil {
		t.Fatalf("marker of another group accepted: %v %v", step, err)
	}
	if step, err := readStepSession(p.Run, p.Root+1, p.ActorGID); step || err == nil {
		t.Fatalf("marker of another owner accepted: %v %v", step, err)
	}
}

// SPEC: The production attestor applies the step policy itself: a verified
// checkout leaves the marker, the drop-in and the Git file for the next
// session, a refused check removes them.
func TestWorkspaceAttestorAppliesStepPolicy(t *testing.T) {
	p := stepTestPolicy(t)
	quiesce := func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
	a := newWorkspaceAttestor(relay.ClientConfig{ActorUID: "actor-1", Generation: 7, Token: attestToken}, WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway", GitIdentity: attestIdentity}, quiesce, p)
	var refuse error
	a.Checkout = func(_ context.Context, r sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) {
		return sessionctl.CheckoutFacts{Commit: r.Commit}, refuse
	}
	a.Ensure = func(context.Context) error { return nil }
	policy := []string{filepath.Join(p.Run, stepSessionFile), p.dropIn(), filepath.Join(p.Run, stepGitConfigFile)}
	if r := attestCall(a, attestArgs(nil)); !r.OK {
		t.Fatalf("attestation: %+v", r)
	}
	for _, path := range policy {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("step policy %s not applied: %v", path, err)
		}
	}
	if data := readTestFile(t, policy[2]); !strings.Contains(data, attestIdentity.Email) {
		t.Fatalf("step Git configuration without the bootstrap identity: %q", data)
	}
	refuse = errors.New("workspace has local changes")
	if r := attestCall(a, attestArgs(nil)); r.OK {
		t.Fatalf("refused check attested: %+v", r)
	}
	for _, path := range policy {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("step policy %s left after a refused check: %v", path, err)
		}
	}
}

// SPEC: A runtime start returns the actor to a free session: the policy of a
// step before the CT restart is removed after the workspace is prepared.
func TestRuntimeStartClearsStepPolicy(t *testing.T) {
	p := stepTestPolicy(t)
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(privateTemp(t), "workspace")
	if err := systemDependencies(workspace, os.Geteuid(), os.Getegid(), p).Workspace(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(p.Run, stepSessionFile), p.dropIn(), filepath.Join(p.Run, stepGitConfigFile)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s left after the runtime start: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "project")); err != nil {
		t.Fatalf("workspace not prepared: %v", err)
	}
}

// SPEC: A Claude Code managed settings directory that is a link or that group
// or others may write stops the runtime start even without a past step, as a
// writable /workspace does: whoever writes it sets the managed hooks and env
// the step session keeps, so no attestation could hold there. The refusal
// names the directory, the reason and the fix; a missing one is fine.
func TestRuntimeStartRefusesUnsafeManagedDirectory(t *testing.T) {
	outside := privateTemp(t)
	for name, setup := range map[string]func(p stepPolicy){
		"group-writable": func(p stepPolicy) { os.Mkdir(p.Managed, 0755); os.Chmod(p.Managed, 0775) },
		"writable drop-in dir": func(p stepPolicy) {
			os.MkdirAll(filepath.Dir(p.dropIn()), 0755)
			os.Chmod(filepath.Dir(p.dropIn()), 0777)
		},
		"linked": func(p stepPolicy) { os.Symlink(outside, p.Managed) },
	} {
		t.Run(name, func(t *testing.T) {
			p := stepTestPolicy(t)
			setup(p)
			err := systemDependencies(filepath.Join(privateTemp(t), "workspace"), os.Geteuid(), os.Getegid(), p).Workspace()
			if err == nil || !strings.Contains(err.Error(), p.Managed) || !strings.Contains(err.Error(), "writable by group or others") || !strings.Contains(err.Error(), "0755") {
				t.Fatalf("unsafe managed settings directory: %v", err)
			}
			if err := p.enter(attestIdentity); err == nil || !strings.Contains(err.Error(), p.Managed) {
				t.Fatalf("step policy refusal without the directory: %v", err)
			}
		})
	}
}

func TestStepPolicyPutsShellStartupFilesAside(t *testing.T) {
	p := stepTestPolicy(t)
	payload := filepath.Join(p.Home, "payload.sh")
	files := map[string]string{
		".bashrc":       "alias git=/workspace/home/payload.sh\n",
		".profile":      "PATH=$HOME/bin:$PATH\n",
		".bash_profile": "source ~/payload.sh\n",
		".bash_logout":  "~/payload.sh\n",
		".gitconfig":    "[core]\n\tfsmonitor = /workspace/home/payload.sh\n",
		// Резервная копия прошлого шага в ту же секунду не перезаписывается.
		".bashrc.brood-" + stepTestStamp: "earlier step\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(p.Home, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(payload, filepath.Join(p.Home, ".bash_login")); err != nil {
		t.Fatal(err)
	}
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	// Образ rc-файлов в home не содержит: после окна тишины их нет.
	for _, name := range actorShellStartupFiles {
		if _, err := os.Lstat(filepath.Join(p.Home, name)); !os.IsNotExist(err) {
			t.Fatalf("%s left in the step session home: %v", name, err)
		}
	}
	for name, want := range map[string]string{
		".bashrc.brood-" + stepTestStamp + ".1": files[".bashrc"],
		".bashrc.brood-" + stepTestStamp:        "earlier step\n",
		".profile.brood-" + stepTestStamp:       files[".profile"],
		".bash_profile.brood-" + stepTestStamp:  files[".bash_profile"],
		".bash_logout.brood-" + stepTestStamp:   files[".bash_logout"],
		".gitconfig":                            files[".gitconfig"],
	} {
		if data, err := os.ReadFile(filepath.Join(p.Home, name)); err != nil || string(data) != want {
			t.Fatalf("%s: %q %v", name, data, err)
		}
	}
	// Ссылка переносится как ссылка: root не идёт по ней.
	if target, err := os.Readlink(filepath.Join(p.Home, ".bash_login.brood-"+stepTestStamp)); err != nil || target != payload {
		t.Fatalf("symlink moved as %q %v", target, err)
	}
	before := entries(t, p.Home)
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	if after := entries(t, p.Home); !slices.Equal(before, after) {
		t.Fatalf("home changed without startup files: %v -> %v", before, after)
	}
}

func TestStepPolicyRefusesForeignHome(t *testing.T) {
	p := stepTestPolicy(t)
	p.ActorUID = p.Root + 1
	if err := p.enter(attestIdentity); err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("home of another owner accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(p.Run, stepSessionFile)); !os.IsNotExist(err) {
		t.Fatalf("step marker left after refusal: %v", err)
	}
	if _, err := os.Lstat(p.dropIn()); !os.IsNotExist(err) {
		t.Fatalf("managed settings left after refusal: %v", err)
	}
}

func TestStepGitConfigCarriesOnlyIdentity(t *testing.T) {
	list := func(t *testing.T, path string) string {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", "config", "--file", path, "--null", "--list")
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git config --list: %v", err)
		}
		return string(out)
	}
	p := stepTestPolicy(t)
	path := filepath.Join(p.Run, stepGitConfigFile)
	identity := gitidentity.Identity{Name: `brood "w2" \ #3; x`, Email: "w2@brood.corp.example"}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := p.enter(identity); err != nil {
		t.Fatal(err)
	}
	if got, want := list(t, path), "user.name\n"+identity.Name+"\x00user.email\n"+identity.Email+"\x00"; got != want {
		t.Fatalf("step Git configuration %q, want %q", got, want)
	}
	if err := p.enter(gitidentity.Identity{}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 0 || list(t, path) != "" {
		t.Fatalf("configuration without identity: %q %v", data, err)
	}
}

func stepTestLayout(t *testing.T, p stepPolicy) appLayout {
	t.Helper()
	if err := publishProjectSecrets(p.Run, projectsecret.Bundle{Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}}, p.Root, p.ActorGID); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkspaceOrigin(p.Run, WorkspaceOrigin{}, p.Root, p.ActorGID); err != nil {
		t.Fatal(err)
	}
	return appLayout{Project: privateTemp(t), Home: p.Home, Run: p.Run, RunUID: p.Root, RunGID: p.ActorGID}
}

func TestStepSessionSelectsClaudeLaunch(t *testing.T) {
	p := stepTestPolicy(t)
	layout := stepTestLayout(t, p)
	global := filepath.Join(p.Home, ".claude.json")
	actorConfig := `{"env":{"PATH":"/workspace/home/bin:/usr/bin","LD_PRELOAD":"/workspace/home/payload.so"},"theme":"light"}`
	if err := os.WriteFile(global, []byte(actorConfig), 0600); err != nil {
		t.Fatal(err)
	}
	launch := func() ([]string, []string) {
		t.Helper()
		var warnings bytes.Buffer
		args, env, err := prepareClaudeLaunch(layout, &warnings)
		if err != nil || warnings.Len() != 0 {
			t.Fatalf("launch: %v %q", err, warnings.String())
		}
		return args, env
	}
	freeEnv := append(actorEnv(), "SERVICE_KEY=synthetic-env")
	// Свободная сессия — запуск до #224: MCP и окружение актора сохраняются.
	args, env := launch()
	if !reflect.DeepEqual(args, projectAppArgs()) || !reflect.DeepEqual(env, freeEnv) {
		t.Fatalf("free session launch changed: %q %q", args, env)
	}
	if config := readJSONObject(t, global); config["env"] == nil {
		t.Fatal("free session removed the actor env of ~/.claude.json")
	}
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	args, env = launch()
	if !reflect.DeepEqual(args, stepAppArgs()) {
		t.Fatalf("step session argv %q", args)
	}
	if want := append(slices.Clone(freeEnv), "GIT_CONFIG_GLOBAL="+filepath.Join(p.Run, stepGitConfigFile), "SHELL=/bin/bash"); !reflect.DeepEqual(env, want) {
		t.Fatalf("step session env %q, want %q", env, want)
	}
	// Claude Code 2.1.310 применяет env из ~/.claude.json при любом
	// --setting-sources: перед сессией шага он уходит в копию рядом.
	config := readJSONObject(t, global)
	if _, kept := config["env"]; kept || config["theme"] != "light" {
		t.Fatalf("step session ~/.claude.json: %v", config)
	}
	aside, _ := filepath.Glob(global + ".env.brood-*")
	if len(aside) != 1 {
		t.Fatalf("env copy: %v", aside)
	}
	var kept map[string]string
	if data, err := os.ReadFile(aside[0]); err != nil || json.Unmarshal(data, &kept) != nil || kept["LD_PRELOAD"] != "/workspace/home/payload.so" || len(kept) != 2 {
		t.Fatalf("env copy content: %q %v", data, err)
	}
	if info, err := os.Lstat(aside[0]); err != nil || info.Mode() != 0600 {
		t.Fatalf("env copy mode: %v %v", info, err)
	}
	// Повторный старт сессии шага без env ничего не переносит.
	launch()
	if again, _ := filepath.Glob(global + ".env.brood-*"); len(again) != 1 {
		t.Fatalf("env copied again: %v", again)
	}
	if err := p.leave(); err != nil {
		t.Fatal(err)
	}
	if args, env := launch(); !reflect.DeepEqual(args, projectAppArgs()) || !reflect.DeepEqual(env, freeEnv) {
		t.Fatalf("launch after leaving the step: %q %q", args, env)
	}
}

// claudeGlobalConfigPath repeats the choice of pinned Claude Code 2.1.310
// (y_ in the binary, checked by an offline launch): ~/.claude/.config.json,
// followed through a link, wins over ~/.claude.json when it is a regular file
// that is non-empty, or writable while ~/.claude.json is missing.
func claudeGlobalConfigPath(home string) string {
	legacy, current := filepath.Join(home, ".claude", ".config.json"), filepath.Join(home, ".claude.json")
	if info, err := os.Stat(legacy); err == nil && info.Mode().IsRegular() {
		_, missing := os.Stat(current)
		if info.Size() > 0 || (info.Mode()&0222 != 0 && os.IsNotExist(missing)) {
			return legacy
		}
	}
	return current
}

// FEATURE: No global Claude configuration of the actor carries env into the
// step session (#224).
// SPEC: Claude Code 2.1.310 applies the env of the global configuration it
// selects and of server-managed settings cached in ~/.claude at any
// --setting-sources. The step session starts with ~/.claude/.config.json and
// ~/.claude/remote-settings.json put aside as they are, links included, so
// the selected global configuration is ~/.claude.json without env; the free
// session keeps both files.
func TestStepSessionPutsLegacyClaudeConfigAside(t *testing.T) {
	payload := `{"env":{"GIT_CONFIG_GLOBAL":"/workspace/home/.gitconfig","LD_PRELOAD":"/workspace/home/payload.so","BASH_ENV":"/workspace/home/payload.sh"},"theme":"light"}`
	remote := `{"env":{"BASH_ENV":"/workspace/home/payload.sh"},"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/workspace/home/payload.sh"}]}]}}`
	for name, setup := range map[string]func(t *testing.T, home string){
		"both configs": func(t *testing.T, home string) {
			writeTestFile(t, filepath.Join(home, ".claude.json"), `{"env":{"PATH":"/workspace/home/bin"},"theme":"light"}`)
			writeTestFile(t, filepath.Join(home, ".claude", ".config.json"), payload)
		},
		"legacy only": func(t *testing.T, home string) {
			writeTestFile(t, filepath.Join(home, ".claude", ".config.json"), payload)
		},
		"empty legacy": func(t *testing.T, home string) {
			writeTestFile(t, filepath.Join(home, ".claude", ".config.json"), "")
		},
		"linked legacy": func(t *testing.T, home string) {
			writeTestFile(t, filepath.Join(home, ".claude.json"), `{"theme":"light"}`)
			writeTestFile(t, filepath.Join(home, "elsewhere.json"), payload)
			if err := os.Symlink(filepath.Join(home, "elsewhere.json"), filepath.Join(home, ".claude", ".config.json")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := stepTestPolicy(t)
			layout := stepTestLayout(t, p)
			if err := os.Mkdir(filepath.Join(p.Home, ".claude"), 0700); err != nil {
				t.Fatal(err)
			}
			setup(t, p.Home)
			writeTestFile(t, filepath.Join(p.Home, ".claude", "remote-settings.json"), remote)
			aside := map[string]string{}
			for _, name := range []string{".config.json", "remote-settings.json"} {
				if target, err := os.Readlink(filepath.Join(p.Home, ".claude", name)); err == nil {
					aside[name] = "link:" + target
				} else {
					aside[name] = readTestFile(t, filepath.Join(p.Home, ".claude", name))
				}
			}
			launch := func() {
				t.Helper()
				if _, _, err := prepareClaudeLaunch(layout, io.Discard); err != nil {
					t.Fatalf("launch: %v", err)
				}
			}
			launch()
			for name := range aside {
				if _, err := os.Lstat(filepath.Join(p.Home, ".claude", name)); err != nil {
					t.Fatalf("free session moved ~/.claude/%s: %v", name, err)
				}
			}
			if err := p.enter(attestIdentity); err != nil {
				t.Fatal(err)
			}
			launch()
			if selected := claudeGlobalConfigPath(p.Home); selected != filepath.Join(p.Home, ".claude.json") {
				t.Fatalf("step session global configuration is %s", selected)
			}
			if config := readJSONObject(t, filepath.Join(p.Home, ".claude.json")); config["env"] != nil {
				t.Fatalf("step session global configuration keeps env: %v", config)
			}
			for name, want := range aside {
				if _, err := os.Lstat(filepath.Join(p.Home, ".claude", name)); !os.IsNotExist(err) {
					t.Fatalf("~/.claude/%s left in the step session: %v", name, err)
				}
				copies, _ := filepath.Glob(filepath.Join(p.Home, ".claude", name+".brood-*"))
				if len(copies) != 1 {
					t.Fatalf("~/.claude/%s copies: %v", name, copies)
				}
				got := ""
				if target, err := os.Readlink(copies[0]); err == nil {
					got = "link:" + target
				} else {
					got = readTestFile(t, copies[0])
				}
				if got != want {
					t.Fatalf("~/.claude/%s copy %q, want %q", name, got, want)
				}
			}
		})
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStepSessionFailsClosed(t *testing.T) {
	marker := func(t *testing.T, p stepPolicy, content string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(p.Run, stepSessionFile)
		os.Remove(path)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, setup := range map[string]func(t *testing.T, p stepPolicy){
		"other mode":    func(t *testing.T, p stepPolicy) { marker(t, p, string(stepSessionMarker), 0640) },
		"other content": func(t *testing.T, p stepPolicy) { marker(t, p, `{"mode":"free"}`+"\n", 0440) },
		"symlinked marker": func(t *testing.T, p stepPolicy) {
			os.Symlink(filepath.Join(p.Home, "x"), filepath.Join(p.Run, stepSessionFile))
		},
		"linked env config": func(t *testing.T, p stepPolicy) {
			p.enter(attestIdentity)
			os.Symlink(filepath.Join(p.Run, "x"), filepath.Join(p.Home, ".claude.json"))
		},
		// Claude идёт по ссылке ~/.claude: её .config.json лежал бы вне home.
		"linked claude directory": func(t *testing.T, p stepPolicy) {
			p.enter(attestIdentity)
			elsewhere := filepath.Join(filepath.Dir(p.Home), "elsewhere")
			os.Mkdir(elsewhere, 0700)
			os.Symlink(elsewhere, filepath.Join(p.Home, ".claude"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := stepTestPolicy(t)
			layout := stepTestLayout(t, p)
			setup(t, p)
			var warnings bytes.Buffer
			if args, env, err := prepareClaudeLaunch(layout, &warnings); err == nil {
				t.Fatalf("Claude launched without a proven session mode: %q %q", args, env)
			}
		})
	}
}

func TestSystemStepPolicyUsesClaudeManagedLocation(t *testing.T) {
	p := systemStepPolicy()
	// /etc/claude-code — путь управляемых настроек Claude Code на Linux; в
	// закреплённой 2.1.310 переопределение пути вырезано из сборки.
	if p.dropIn() != "/etc/claude-code/managed-settings.d/"+stepManagedFile || p.Run != "/run/brood" || p.Home != "/workspace/home" {
		t.Fatalf("system step policy paths: %+v", p)
	}
	if p.Root != 0 || p.RootGID != 0 || p.ActorGID != 1000 || p.ActorUID != 1000 {
		t.Fatalf("system step policy owners: %+v", p)
	}
}
