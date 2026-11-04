//go:build linux

// FEATURE: Browser detach and reconnect preserve the core host's real PTY child.
// SPEC: Attachment never ensures/stops the child; shell state and PID survive.
// IMPLEMENTATION: Run attaches to a real Host running a harmless stateful shell.
// RESULT: IMPLEMENTED — reconnect retains PID, proof, and shell counter under -race.
// STATUS: DONE
package ptyattach

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"golang.org/x/sys/unix"
)

func TestRunCoreHostRetainsChildAndShellState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	host, err := ptysession.New(ptysession.Config{
		Command: []string{"/bin/sh", "-c", `stty raw -echo; n=0; printf 'ATTACH_READY'; while IFS= read -r line; do n=$((n+1)); printf 'STATE:%s:%s\n' "$n" "$line"; done`},
		Env:     []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color"},
		Columns: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		host.Close()
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- host.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		host.Close()
		listener.Close()
		result(t, served)
	})
	call := func(action string) ptysession.Reply {
		t.Helper()
		reply, err := ptysession.Call(ctx, path, ptysession.Request{Action: action})
		if err != nil || !reply.OK {
			t.Fatalf("%s: %+v, %v", action, reply, err)
		}
		return reply
	}
	initial := call("ensure")
	if !initial.State.Running || initial.State.PID <= 0 {
		t.Fatalf("no child: %+v", initial)
	}
	attach := func() (*os.File, context.CancelFunc, <-chan error) {
		master, slave := terminalPair(t)
		attachCtx, detach := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- Run(attachCtx, path, slave, slave) }()
		t.Cleanup(detach)
		return master, detach, done
	}
	first, detach, done := attach()
	terminalContains(t, first, []byte("ATTACH_READY"))
	if _, err := first.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	terminalContains(t, first, []byte("STATE:1:first\n"))
	if err := unix.IoctlSetWinsize(int(first.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 132, Row: 42}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	resizeDeadline := time.Now().Add(2 * time.Second)
	for {
		state := call("status").State
		if state.Columns == 132 && state.Rows == 42 {
			break
		}
		if time.Now().After(resizeDeadline) {
			t.Fatalf("SIGWINCH never reached host PTY: %+v", state)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	detach()
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
	detached := call("status")
	if !detached.State.Running || detached.State.PID != initial.State.PID || detached.Proof != initial.Proof {
		t.Fatalf("detach changed child: before=%+v after=%+v", initial, detached)
	}
	second, detachSecond, secondDone := attach()
	terminalContains(t, second, []byte("STATE:1:first"))
	if _, err := second.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	terminalContains(t, second, []byte("STATE:2:second\n"))
	detachSecond()
	if err := result(t, secondDone); err != nil {
		t.Fatal(err)
	}
	final := call("status")
	if !final.State.Running || final.State.PID != initial.State.PID {
		t.Fatalf("reconnect replaced child: %+v", final)
	}
}

func terminalContains(t *testing.T, terminal *os.File, want []byte) {
	t.Helper()
	var seen []byte
	for len(seen) < ptysession.MaxMessage {
		seen = append(seen, readTerminal(t, terminal, 1)...)
		if bytes.HasSuffix(seen, want) {
			return
		}
	}
	t.Fatalf("terminal did not contain %q", want)
}
