//go:build linux

package ptysession

// FEATURE: Сохраняемый PTY с read-only VT observer.
// SPEC: Detach сохраняет процесс, байты не меняются, proof ограждает мутации,
// браузерный writer исключает остальных писателей.
// IMPLEMENTATION: Настоящий /dev/ptmx, единый mutex для snapshot/subscription,
// ограниченные очереди и независимый headless VT с закрытой response pipe.
// RESULT: IMPLEMENTED — реальные PTY, race и полный Go suite прошли.
// STATUS: DONE

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
)

func TestPTYFixture(t *testing.T) {
	if os.Getenv("BROOD_PTY_FIXTURE") != "1" {
		return
	}
	if _, err := term.MakeRaw(0); err != nil {
		os.Exit(21)
	}
	initial, _ := base64.StdEncoding.DecodeString(os.Getenv("INITIAL"))
	if string(initial) == "fixture:flood" || string(initial) == "fixture:size" {
		_, _ = os.Stdout.Write([]byte("ready"))
		var trigger [1]byte
		_, _ = os.Stdin.Read(trigger[:])
		if string(initial) == "fixture:flood" {
			floodUntilInput()
		} else {
			columns, rows, _ := term.GetSize(0)
			_, _ = fmt.Fprintf(os.Stdout, "size=%dx%d", columns, rows)
		}
	} else {
		_, _ = os.Stdout.Write(initial)
	}
	_, _ = io.Copy(os.Stdout, os.Stdin)
	os.Exit(0)
}

// floodUntilInput пишет вывод, пока не придёт следующий байт ввода, и
// сообщает записанный объём. Длину потока задаёт тест, а не константа:
// буферы host и сокета (SO_SNDBUF задаёт машина) переполняются при
// любом размере, а под race VT observer не разбирает лишние мегабайты.
func floodUntilInput() {
	stop := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		close(stop)
	}()
	chunk := []byte(strings.Repeat("x", 4096))
	flooded := 0
	for {
		select {
		case <-stop:
			// Число стоит перед маркером: экран с "finished" содержит его целиком.
			_, _ = fmt.Fprintf(os.Stdout, "\r\nflooded=%d finished", flooded)
			return
		default:
			n, _ := os.Stdout.Write(chunk)
			flooded += n
		}
	}
}

func fixture(t *testing.T, initial string) (*Host, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Command: []string{exe, "-test.run=^TestPTYFixture$"}, Env: []string{"BROOD_PTY_FIXTURE=1", "INITIAL=" + base64.StdEncoding.EncodeToString([]byte(initial))}, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "pty.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		_ = h.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not exit")
		}
	})
	return h, socket
}
func rpc(t *testing.T, path string, r Request) Reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reply, err := Call(ctx, path, r)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}
func mustOK(t *testing.T, r Reply) Reply {
	t.Helper()
	if !r.OK {
		t.Fatalf("request refused: %+v", r)
	}
	return r
}
func awaitScreen(t *testing.T, path, proof, want string) Reply {
	t.Helper()
	// Под race VT observer разбирает поток примерно в 10 раз медленнее,
	// а flood-тест гонит через него сотни KiB.
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		r := mustOK(t, rpc(t, path, Request{Action: "screen", Proof: proof}))
		if strings.Contains(r.Screen, want) {
			return r
		}
		select {
		case <-deadline.C:
			t.Fatalf("screen missing %q: %q", want, r.Screen)
		case <-tick.C:
		}
	}
}
func attach(t *testing.T, path string, readOnly bool) (net.Conn, *bufio.Reader, Reply) {
	t.Helper()
	c, e := net.Dial("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	if e = WriteMessage(c, Request{Action: "attach", ReadOnly: readOnly}); e != nil {
		t.Fatal(e)
	}
	br := bufio.NewReader(c)
	var r Reply
	if e = ReadMessage(br, &r); e != nil {
		t.Fatal(e)
	}
	return c, br, r
}
func TestPersistentPTYAndProof(t *testing.T) {
	_, path := fixture(t, "ready")
	if r := mustOK(t, rpc(t, path, Request{Action: "status"})); r.State.Running {
		t.Fatal("New started child")
	}
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	if r.Proof == "" || r.State.PID == 0 || r.State.Start == "" || r.State.Epoch == "" {
		t.Fatalf("missing ownership: %+v", r)
	}
	awaitScreen(t, path, r.Proof, "ready")
	again := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	if again.State.PID != r.State.PID || again.Proof != r.Proof {
		t.Fatal("ensure replaced child")
	}
	c, _, a := attach(t, path, false)
	mustOK(t, a)
	if !bytes.Contains(a.Replay, []byte("ready")) {
		t.Fatalf("missing replay: %q", a.Replay)
	}
	_ = c.Close()
	var in Reply
	for i := 0; i < 100; i++ {
		in = rpc(t, path, Request{Action: "input", Proof: r.Proof, Data: []byte("persist")})
		if in.OK {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mustOK(t, in)
	awaitScreen(t, path, r.Proof, "readypersist")
	_, _, a = attach(t, path, true)
	mustOK(t, a)
	if a.State.PID != r.State.PID {
		t.Fatal("detach replaced child")
	}
	if s := rpc(t, path, Request{Action: "stop", Proof: "stale"}); s.OK || s.Outcome != "refused" {
		t.Fatalf("stale stop: %+v", s)
	}
	mustOK(t, rpc(t, path, Request{Action: "stop", Proof: r.Proof}))
	if rpc(t, path, Request{Action: "status"}).State.Running {
		t.Fatal("stop did not confirm exit")
	}
	next := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	if next.Proof == r.Proof {
		t.Fatal("proof reused")
	}
	for _, action := range []string{"input", "resize", "screen", "tail", "stop"} {
		if s := rpc(t, path, Request{Action: action, Proof: r.Proof, Columns: 90, Rows: 30, Lines: 5}); s.OK {
			t.Fatalf("old proof accepted by %s", action)
		}
	}
}
func TestRawOutputWriterArbitrationAndObserver(t *testing.T) {
	_, path := fixture(t, "ready\x1b[?1003h\x1b[?1006h\x1b[?2004h\x1b[6n")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	c, br, a := attach(t, path, false)
	mustOK(t, a)
	for _, mode := range []string{"?1003h", "?1006h", "?2004h"} {
		if !bytes.Contains(a.Replay, []byte(mode)) {
			t.Fatalf("missing mode %s in %q", mode, a.Replay)
		}
	}
	_, _, second := attach(t, path, false)
	if second.OK || !strings.Contains(second.Error, "writer") {
		t.Fatalf("second writer: %+v", second)
	}
	for _, action := range []string{"input", "resize"} {
		s := rpc(t, path, Request{Action: action, Proof: r.Proof, Data: []byte("bad"), Columns: 90, Rows: 30})
		if s.OK || !strings.Contains(s.Error, "writer") {
			t.Fatalf("RPC bypassed writer: %+v", s)
		}
	}
	viewer, _, v := attach(t, path, true)
	mustOK(t, v)
	if err := WriteMessage(viewer, Frame{Type: "resize", Columns: 100, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	raw := []byte("\x1b[?1049h\x1b[2J\x1b[3;7Hraw\x1b[6n\xff\x00")
	if err := WriteMessage(c, Frame{Type: "input", Data: raw}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for len(got) < len(raw) {
		var f Frame
		if err := ReadMessage(br, &f); err != nil {
			t.Fatal(err)
		}
		if f.Type != "output" {
			t.Fatalf("unexpected frame: %+v", f)
		}
		got = append(got, f.Data...)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("raw stream changed: %q != %q", got, raw)
	}
	awaitScreen(t, path, r.Proof, "raw")
	state := rpc(t, path, Request{Action: "status"})
	if state.State.Columns != 80 || state.State.Rows != 24 {
		t.Fatal("viewer resized child")
	}
	// Ответ observer на DSR fixture вернула бы эхом в этот поток.
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var f Frame
	if err := ReadMessage(br, &f); err == nil {
		t.Fatalf("observer injected output: %+v", f)
	}
}
func TestValidationAndClose(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty command accepted")
	}
	h, path := fixture(t, "ready")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	for _, request := range []Request{{Action: "unknown"}, {Action: "resize", Proof: r.Proof, Columns: 19, Rows: 5}, {Action: "tail", Proof: r.Proof, Lines: 0}, {Action: "tail", Proof: r.Proof, Lines: 10001}} {
		if s := rpc(t, path, request); s.OK || s.Outcome != "refused" {
			t.Fatalf("invalid request accepted: %+v", s)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked")
	}
}
