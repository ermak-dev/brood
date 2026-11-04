//go:build linux

// FEATURE: Fixed browser attachment forwards a real terminal without interpreting it.
// SPEC: Exact input/output bytes, raw TTY restoration, serialized resize/input,
// bounded newline JSON, explicit refusal, and cancellation without session stop.
// IMPLEMENTATION: Run uses a single input/resize writer and cancellable raw output.
// RESULT: IMPLEMENTED — real PTY and local protocol tests pass under -race.
// STATUS: DONE
package ptyattach

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type wireMessage struct {
	Action   string `json:"action,omitempty"`
	Type     string `json:"type,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Columns  int    `json:"columns,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

func terminalPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 100, Row: 30}); err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func attachSocket(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pty.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	return path, accepted
}

func connection(t *testing.T, accepted <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case conn := <-accepted:
		t.Cleanup(func() { conn.Close() })
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		return conn
	case <-time.After(5 * time.Second):
		t.Fatal("attach did not connect")
		return nil
	}
}

func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("attachment did not exit")
		return nil
	}
}

func readTerminal(t *testing.T, file *os.File, size int) []byte {
	t.Helper()
	got := make([]byte, size)
	offset := 0
	deadline := time.Now().Add(5 * time.Second)
	for offset < size {
		if time.Now().After(deadline) {
			t.Fatalf("terminal read timeout: got %q", got[:offset])
		}
		fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			continue
		}
		n, err = file.Read(got[offset:])
		if err != nil {
			t.Fatal(err)
		}
		offset += n
	}
	return got
}

func TestRunRealTerminalBytesResizeAndRestore(t *testing.T) {
	master, slave := terminalPair(t)
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	path, accepted := attachSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slave, slave) }()
	conn := connection(t, accepted)
	decoder := json.NewDecoder(conn)
	var request wireMessage
	if err := decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	if request.Action != "attach" || request.ReadOnly || request.Columns != 100 || request.Rows != 30 {
		t.Fatalf("bad attach request: %+v", request)
	}
	replay := []byte("\x1b[?1049h\x1b[2Jreplay\r\n")
	reply, _ := json.Marshal(map[string]any{"ok": true, "outcome": "complete", "replay": replay})
	if _, err := conn.Write(append(reply, '\n')); err != nil {
		t.Fatal(err)
	}
	if got := readTerminal(t, master, len(replay)); !bytes.Equal(got, replay) {
		t.Fatalf("replay changed: %q", got)
	}
	raw, _ := term.GetState(int(slave.Fd()))
	if reflect.DeepEqual(before, raw) {
		t.Fatal("terminal was not made raw")
	}
	output := []byte{'\x1b', '[', '6', 'n', 0, 0xff, '\n', '\r', 0xc3, 0xa9}
	if err := json.NewEncoder(conn).Encode(wireMessage{Type: "output", Data: output}); err != nil {
		t.Fatal(err)
	}
	if got := readTerminal(t, master, len(output)); !bytes.Equal(got, output) {
		t.Fatalf("output changed: %v", got)
	}
	input := []byte{3, 4, 26, 0, 0xff, '\r', '\n', '\x1b', '[', 'A'}
	if _, err := master.Write(input); err != nil {
		t.Fatal(err)
	}
	var gotInput []byte
	for len(gotInput) < len(input) {
		var frame wireMessage
		if err := decoder.Decode(&frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type != "input" {
			t.Fatalf("unexpected frame: %+v", frame)
		}
		gotInput = append(gotInput, frame.Data...)
	}
	if !bytes.Equal(gotInput, input) {
		t.Fatalf("input changed: %v", gotInput)
	}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 132, Row: 42}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	var resize wireMessage
	if err := decoder.Decode(&resize); err != nil {
		t.Fatal(err)
	}
	if resize.Type != "resize" || resize.Columns != 132 || resize.Rows != 42 {
		t.Fatalf("bad resize: %+v", resize)
	}
	cancel()
	if err := result(t, done); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal state not restored: %v", err)
	}
	var extra wireMessage
	if err := decoder.Decode(&extra); err == nil {
		t.Fatalf("cancellation sent mutation: %+v", extra)
	}
}

func TestRunRefusesWriterAndRestoresTerminal(t *testing.T) {
	_, slave := terminalPair(t)
	before, _ := term.GetState(int(slave.Fd()))
	path, accepted := attachSocket(t)
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), path, slave, slave) }()
	conn := connection(t, accepted)
	var request wireMessage
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "{\"ok\":false,\"outcome\":\"refused\",\"error\":\"writer conflict\"}\n")
	if err := result(t, done); err == nil || !strings.Contains(err.Error(), "writer conflict") {
		t.Fatalf("expected writer conflict: %v", err)
	}
	after, _ := term.GetState(int(slave.Fd()))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refusal left raw terminal")
	}
}

func TestRunBoundsUnterminatedReply(t *testing.T) {
	_, slave := terminalPair(t)
	path, accepted := attachSocket(t)
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), path, slave, slave) }()
	conn := connection(t, accepted)
	if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	go func() { io.WriteString(conn, strings.Repeat(" ", ptysession.MaxMessage+1)) }()
	if err := result(t, done); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected frame limit: %v", err)
	}
}

func TestRunEOFOnlyDetaches(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	path, accepted := attachSocket(t)
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), path, input, output) }()
	conn := connection(t, accepted)
	decoder := json.NewDecoder(conn)
	var request wireMessage
	if err := decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	if request.Action != "attach" || request.Columns < 20 || request.Rows < 5 {
		t.Fatalf("unsafe initial size: %+v", request)
	}
	io.WriteString(conn, "{\"ok\":true,\"outcome\":\"complete\"}\n")
	writer.Close()
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
	var unexpected wireMessage
	if err := decoder.Decode(&unexpected); err == nil {
		t.Fatalf("EOF sent mutation: %+v", unexpected)
	}
}

func TestRunCancellationWithBlockedOutput(t *testing.T) {
	_, slave := terminalPair(t)
	before, _ := term.GetState(int(slave.Fd()))
	outputReader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outputReader.Close()
	defer output.Close()
	path, accepted := attachSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slave, output) }()
	conn := connection(t, accepted)
	var request wireMessage
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		t.Fatal(err)
	}
	reply, _ := json.Marshal(map[string]any{"ok": true, "outcome": "complete", "replay": bytes.Repeat([]byte("x"), 128<<10)})
	if _, err := conn.Write(append(reply, '\n')); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		count, err := unix.IoctlGetInt(int(outputReader.Fd()), unix.TIOCINQ)
		if err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attachment never wrote replay")
		}
		select {
		case <-done:
			t.Fatal("attachment exited before replay")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
	after, _ := term.GetState(int(slave.Fd()))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("blocked output prevented raw restoration")
	}
}

func TestRunRejectsNilFiles(t *testing.T) {
	if err := Run(context.Background(), "/unused", nil, nil); err == nil {
		t.Fatal("nil files accepted")
	}
}
