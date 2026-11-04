//go:build linux

package sessionctl

// FEATURE: Live session proof binds ccmsg credentials to the held PTY child.
// SPEC: Real socket credentials, procStart, exact PID/start, foreground TTY and
// holder epoch/proof must agree; registry claims cannot select a different child.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

// TestPTYIdentityChild is a harmless local ccmsg fixture, never a model process.
func TestPTYIdentityChild(t *testing.T) {
	if os.Getenv("SESSIONCTL_IDENTITY_CHILD") != "1" {
		return
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	cwd, _ := os.Getwd()
	socket := filepath.Join(config, "ccmsg.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	proc, err := processInfo(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := json.Marshal(map[string]string{"procStart": proc.start})
	if err = os.WriteFile(filepath.Join(config, "sessions", fmt.Sprintf("%d.%s.key", os.Getpid(), digest([]byte(socket)))), key, 0600); err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "fixture", "cwd": cwd, "messagingSocketPath": socket})
	if err = os.WriteFile(filepath.Join(config, "sessions", "registry.json"), row, 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Print("\x1b[2J\x1b[Hready\n")
	var b [1]byte
	for {
		if _, err = os.Stdin.Read(b[:]); err != nil {
			return
		}
	}
}

func TestIdentityWithRealPTYAndCCMSG(t *testing.T) {
	root, err := os.MkdirTemp("", "brood-identity-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	config, cwd := filepath.Join(root, "config"), filepath.Join(root, "project")
	os.MkdirAll(filepath.Join(config, "sessions"), 0700)
	os.Mkdir(cwd, 0700)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run, _ := realPTYFixture(t, []string{exe, "-test.run=^TestPTYIdentityChild$"}, cwd, append(os.Environ(), "SESSIONCTL_IDENTITY_CHILD=1", "CLAUDE_CONFIG_DIR="+config))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = ensureSession(ctx, run); err != nil {
		t.Fatal(err)
	}
	var id identity
	if err = workspaceWait(ctx, 3*time.Second, func(ctx context.Context) (bool, error) {
		var e error
		id, e = proveIdentity(ctx, config, cwd, run, true)
		return e == nil, nil
	}); err != nil {
		t.Fatal("fixture did not become proven", err)
	}
	holder, err := run(ctx, ptysession.Request{Action: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if id.PID != holder.State.PID || id.HolderProof != holder.Proof || id.Epoch != holder.State.Epoch {
		t.Fatalf("proof not bound: %+v %+v", id, holder)
	}
	for _, field := range []string{"pid", "start", "running", "proof", "epoch", "changed"} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			forged := func(ctx context.Context, req ptysession.Request) (ptysession.Reply, error) {
				reply, err := run(ctx, req)
				calls++
				switch field {
				case "pid":
					reply.State.PID++
				case "start":
					reply.State.Start += "1"
				case "running":
					reply.State.Running = false
				case "proof":
					reply.Proof = ""
				case "epoch":
					reply.State.Epoch = ""
				case "changed":
					reply.Proof = strconv.Itoa(calls)
				}
				return reply, err
			}
			if _, err := proveIdentity(ctx, config, cwd, forged, true); err == nil {
				t.Fatal("unproven holder accepted", field)
			}
		})
	}
	keyPath := filepath.Join(config, "sessions", fmt.Sprintf("%d.%s.key", id.PID, digest([]byte(id.Socket))))
	if err = os.WriteFile(keyPath, []byte(`{"procStart":"stale"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = proveIdentity(ctx, config, cwd, run, true); err == nil {
		t.Fatal("stale procStart accepted")
	}
}
