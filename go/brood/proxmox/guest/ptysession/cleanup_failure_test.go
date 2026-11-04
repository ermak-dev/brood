//go:build linux

package ptysession

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// BUG: Неподтверждённая очистка скрывалась за успешным closed/Close.
// SPEC: Клиент получает причину, новый сеанс запрещён; PID остаётся закреплённым.
func cleanupFailureHost(t *testing.T) (*Host, string) {
	t.Helper()
	h, err := newHost(Config{Command: []string{"/bin/sh", "-c", "printf ready; read trigger"}}, func(pid int) error {
		if err := finishPinnedGroup(pid); err != nil {
			return err
		}
		return errors.New("injected cleanup verification failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/pty.sock"
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = h.Serve(context.Background(), l) }()
	t.Cleanup(func() {
		_ = h.Close()
		h.mu.Lock()
		p := h.current
		h.mu.Unlock()
		if p != nil {
			_ = p.cmd.Wait()
		}
	})
	return h, path
}

func TestAttachmentReceivesCleanupFailure(t *testing.T) {
	h, path := cleanupFailureHost(t)
	state := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, state.Proof, "ready")
	c, reader, reply := attach(t, path, true)
	defer c.Close()
	mustOK(t, reply)
	mustOK(t, rpc(t, path, Request{Action: "input", Proof: state.Proof, Data: []byte("\n")}))
	_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
	for {
		var frame Frame
		if err := ReadMessage(reader, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == "output" {
			continue
		}
		if frame.Type != "error" || !strings.Contains(string(frame.Data), "injected cleanup verification failure") {
			t.Fatalf("cleanup uncertainty hidden: %+v", frame)
		}
		break
	}
	held := rpc(t, path, Request{Action: "ensure"})
	if held.OK || held.Outcome != "unknown" || held.State.PID != state.State.PID {
		t.Fatalf("held session restarted or reported success: %+v", held)
	}
	if _, err := os.Stat("/proc/" + strings.Split(state.Proof, ":")[0]); err != nil {
		t.Fatalf("leader PID was released: %v", err)
	}
	if err := h.Close(); err == nil {
		t.Fatal("held cleanup failure lost by Close")
	}
}

func TestCloseReportsCleanupFailureAfterSignal(t *testing.T) {
	h, path := cleanupFailureHost(t)
	state := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, state.Proof, "ready")
	if err := h.Close(); err == nil || !strings.Contains(err.Error(), "injected cleanup verification failure") {
		t.Fatalf("Close lost reaper failure: %v", err)
	}
}
