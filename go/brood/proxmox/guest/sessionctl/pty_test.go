//go:build linux

package sessionctl

// FEATURE: Direct PTY control preserves a single fenced input delivery.
// SPEC: Slash text and its carriage return are one holder-proof-fenced mutation;
// a browser writer conflict refuses before any bytes are delivered.
// IMPLEMENTATION: Typed PTY RPCs retain caller proofs and unknown-after-effect semantics.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

func fixtureIdentity() identity {
	return identity{SessionID: "abc", Process: "boot:12", PID: 123, HolderProof: "holder", Epoch: "epoch"}
}
func liveReply() ptysession.Reply {
	return ptysession.Reply{OK: true, Outcome: "complete", Proof: "holder", State: ptysession.State{PID: 123, Start: "12", Epoch: "epoch", Running: true}}
}

func TestSlashIsOneFencedDelivery(t *testing.T) {
	e, _ := fixture(t)
	deliveries := 0
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		deliveries++
		if req.Action != "input" || req.Proof != "holder" || string(req.Data) != "/status\r" {
			t.Fatalf("unfenced or nonliteral input: %+v", req)
		}
		return liveReply(), nil
	}
	r := call(e, "slash", `{"command":"/status"}`)
	if !r.OK || deliveries != 1 {
		t.Fatalf("slash must be one atomic PTY delivery: response=%+v deliveries=%d", r, deliveries)
	}
}

func TestBrowserWriterConflictRefusesCLIMutations(t *testing.T) {
	for _, op := range []string{"slash", "input", "interrupt", "resize"} {
		t.Run(op, func(t *testing.T) {
			e, _ := fixture(t)
			calls := 0
			e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
				calls++
				return ptysession.Reply{Outcome: "refused", Error: "browser writer conflict"}, nil
			}
			action, args := "session_terminal", `{"action":"`+op+`","proof":"`+terminalProof(fixtureIdentity())+`","data":"hello","cols":80,"rows":24}`
			if op == "slash" {
				action, args = "slash", `{"command":"/status"}`
			}
			r := call(e, action, args)
			if r.OK || r.Outcome == "unknown" || calls != 1 || r.Text != "browser writer conflict" {
				t.Fatalf("writer refusal: %+v calls=%d", r, calls)
			}
		})
	}
}

// FEATURE: Missing holder evidence refuses before contacting the PTY endpoint.
// SPEC: A Claude record alone must never authorize a terminal mutation.
func TestNoMutationWithoutHolderProof(t *testing.T) {
	for _, op := range []string{"slash", "input", "interrupt", "resize"} {
		t.Run(op, func(t *testing.T) {
			e, _ := fixture(t)
			id := fixtureIdentity()
			id.HolderProof = ""
			e.prove = func(context.Context) (identity, error) { return id, nil }
			calls := 0
			e.call = func(context.Context, ptysession.Request) (ptysession.Reply, error) { calls++; return liveReply(), nil }
			action, args := "session_terminal", `{"action":"`+op+`","proof":"`+terminalProof(id)+`","data":"hello","cols":80,"rows":24}`
			if op == "slash" {
				action, args = "slash", `{"command":"/status"}`
			}
			r := call(e, action, args)
			if r.OK || calls != 0 {
				t.Fatalf("unfenced mutation: %+v calls=%d", r, calls)
			}
		})
	}
}

func TestTrustedPTYEndpointRejectsUnsafeOwnershipAndAncestors(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "protected")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pty.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if _, err = trustedPTYSocket(path, uid, gid, root); err != nil {
		t.Fatal(err)
	}
	if _, err = trustedPTYSocket(path, uid+1, gid, root); err == nil {
		t.Fatal("wrong owner accepted")
	}
	if _, err = trustedPTYSocket(path, uid, gid+1, root); err == nil {
		t.Fatal("wrong group accepted")
	}
	os.Chmod(dir, 0770)
	if _, err = trustedPTYSocket(path, uid, gid, root); err == nil {
		t.Fatal("group-writable ancestor accepted")
	}
	os.Chmod(dir, 0700)
	os.Chmod(path, 0666)
	if _, err = trustedPTYSocket(path, uid, gid, root); err == nil {
		t.Fatal("public socket accepted")
	}
	os.Chmod(path, 0660)
	link := filepath.Join(root, "link")
	os.Symlink(dir, link)
	if _, err = trustedPTYSocket(filepath.Join(link, "pty.sock"), uid, gid, root); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if _, err = trustedPTYSocket(path, uid, gid, dir+"/elsewhere"); err == nil {
		t.Fatal("wrong anchor accepted")
	}
}
