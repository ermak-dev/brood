//go:build linux

package ptysession

// FEATURE: Ограниченные очереди attach и владение жизненным циклом PTY.
// SPEC: Медленный reader не блокирует процесс, запись не блокирует control loop;
// resize меняет настоящий slave, Close подтверждает завершение процесса.
// RESULT: IMPLEMENTED — RED shutdown/stop-conflict регрессии исправлены, race прошёл.
// STATUS: DONE

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInputKeepsPTYNonblocking(t *testing.T) {
	h, path := fixture(t, "ready")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	mustOK(t, rpc(t, path, Request{Action: "input", Proof: r.Proof, Data: []byte("x")}))
	mustOK(t, rpc(t, path, Request{Action: "resize", Proof: r.Proof, Columns: 90, Rows: 30}))
	h.mu.Lock()
	raw, err := h.current.master.SyscallConn()
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	_ = raw.Control(func(fd uintptr) { flags, err = unix.FcntlInt(fd, unix.F_GETFL, 0) })
	if err != nil || flags&unix.O_NONBLOCK == 0 {
		t.Fatalf("PTY writes can block control loop: flags=%x err=%v", flags, err)
	}
}
func TestResizeChangesRealPTY(t *testing.T) {
	_, path := fixture(t, "fixture:size")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	mustOK(t, rpc(t, path, Request{Action: "resize", Proof: r.Proof, Columns: 110, Rows: 35}))
	mustOK(t, rpc(t, path, Request{Action: "input", Proof: r.Proof, Data: []byte("x")}))
	awaitScreen(t, path, r.Proof, "size=110x35")
}

func TestSlowReaderDisconnectDoesNotBlockChild(t *testing.T) {
	h, path := fixture(t, "fixture:flood")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	c, _, a := attach(t, path, true)
	mustOK(t, a)
	// Читатель, который не читает, удерживает не больше полной очереди host и
	// SO_SNDBUF сокета. Двойной запас покрывает буферы PTY и поток до второго
	// байта; больше значит, что host держал читателя после переполнения,
	// например до таймаута записи.
	limit := 2 * (queueFrames*maxInput + subscriberSendBuffer(t, h))
	// Первый байт запускает поток, второй останавливает его после отключения:
	// child пишет, пока host не сбросит читателя, сколько бы ни вмещали буферы.
	mustOK(t, rpc(t, path, Request{Action: "input", Proof: r.Proof, Data: []byte("x")}))
	awaitHangup(t, c)
	mustOK(t, rpc(t, path, Request{Action: "input", Proof: r.Proof, Data: []byte("x")}))
	screen := awaitScreen(t, path, r.Proof, "finished").Screen
	var flooded int
	if _, report, _ := strings.Cut(screen, "flooded="); report == "" {
		t.Fatalf("flood size missing: %q", screen)
	} else if _, err := fmt.Sscanf(report, "%d", &flooded); err != nil {
		t.Fatalf("flood size unreadable: %v", err)
	}
	if flooded > limit {
		t.Fatalf("slow reader stayed attached for %d bytes of output, limit %d", flooded, limit)
	}
	h.mu.Lock()
	count := len(h.subscribers)
	h.mu.Unlock()
	if count != 0 {
		t.Fatalf("slow reader remains subscribed: %d", count)
	}
	if !rpc(t, path, Request{Action: "status"}).State.Running {
		t.Fatal("slow reader killed child")
	}
}

// awaitHangup ждёт, пока host закроет свою сторону соединения. Данные не
// читаются: чтение сделало бы читателя быстрым. Для AF_UNIX stream закрытие
// peer видно как POLLRDHUP сразу, даже с непрочитанной очередью; SetReadBuffer
// клиента здесь ничего не ограничивает, очередь учитывает SO_SNDBUF отправителя.
func awaitHangup(t *testing.T, c net.Conn) {
	t.Helper()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	hungUp := false
	deadline := time.Now().Add(15 * time.Second)
	err = raw.Control(func(fd uintptr) {
		for !hungUp && time.Now().Before(deadline) {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLRDHUP}}
			n, pollErr := unix.Poll(fds, 10)
			hungUp = pollErr == nil && n > 0 && fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP) != 0
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hungUp {
		t.Fatal("host kept the slow reader attached")
	}
}

// subscriberSendBuffer возвращает SO_SNDBUF серверной стороны единственной
// подписки: размер задаёт net.core.wmem_default машины, а не код.
func subscriberSendBuffer(t *testing.T, h *Host) int {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscribers) != 1 {
		t.Fatalf("subscribers=%d, want 1", len(h.subscribers))
	}
	size := 0
	for s := range h.subscribers {
		raw, err := s.conn.(*net.UnixConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var sockErr error
		if err = raw.Control(func(fd uintptr) {
			size, sockErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
		}); err == nil {
			err = sockErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return size
}

func TestCloseConfirmsProcessExitAndClosesIdleClients(t *testing.T) {
	h, path := fixture(t, "ready")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, _, reply := attach(t, path, true)
	mustOK(t, reply)
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat("/proc/" + strings.Split(r.Proof, ":")[0]); !os.IsNotExist(err) {
		t.Fatalf("owned child remains after Close: %v", err)
	}
	for _, conn := range []net.Conn{c, a} {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		if _, err = conn.Read(b[:]); err == nil {
			t.Fatal("Close left connection open")
		}
	}
	if err = h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
func TestViewerFrameReceivesExplicitRefusal(t *testing.T) {
	_, path := fixture(t, "ready")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	for i := 0; i < 20; i++ {
		c, reader, a := attach(t, path, true)
		mustOK(t, a)
		if err := WriteMessage(c, Frame{Type: "input", Data: []byte("bad")}); err != nil {
			t.Fatal(err)
		}
		var frame Frame
		if err := ReadMessage(reader, &frame); err != nil {
			t.Fatalf("refusal lost on disconnect: %v", err)
		}
		if frame.Type != "error" || !bytes.Contains(frame.Data, []byte("writer")) {
			t.Fatalf("missing refusal: %+v", frame)
		}
		_ = c.Close()
	}
}

func TestRPCStopDoesNotStealWriter(t *testing.T) {
	_, path := fixture(t, "ready")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	_, _, a := attach(t, path, false)
	mustOK(t, a)
	stopped := rpc(t, path, Request{Action: "stop", Proof: r.Proof})
	if stopped.OK || stopped.Outcome != "refused" || !strings.Contains(stopped.Error, "writer conflict") {
		t.Fatalf("stop stole browser session: %+v", stopped)
	}
	state := mustOK(t, rpc(t, path, Request{Action: "status"}))
	if !state.State.Running || state.Proof != r.Proof {
		t.Fatal("refused stop changed child")
	}
}

func TestListenerFailureClosesAttachments(t *testing.T) {
	h, path := fixture(t, "ready")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	_, _, a := attach(t, path, true)
	mustOK(t, a)
	h.mu.Lock()
	l := h.listener
	h.mu.Unlock()
	_ = l.Close()
	select {
	case <-h.closed:
	case <-time.After(time.Second):
		t.Fatal("listener failure waited for attachments before closing them")
	}
}

func TestRealPTYReplayAndLivePartialBoundary(t *testing.T) {
	_, path := fixture(t, "before\xe2\x82")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "before")
	c, reader, a := attach(t, path, true)
	mustOK(t, a)
	reconstructed := newObserver(80, 24)
	defer reconstructed.terminal.Close()
	reconstructed.write(a.Replay)
	suffix := []byte("\xac\x1b[3;7Hafter")
	mustOK(t, rpc(t, path, Request{Action: "input", Proof: r.Proof, Data: suffix}))
	var live []byte
	for len(live) < len(suffix) {
		var f Frame
		if err := ReadMessage(reader, &f); err != nil {
			t.Fatal(err)
		}
		if f.Type != "output" {
			t.Fatalf("unexpected live frame: %+v", f)
		}
		live = append(live, f.Data...)
		reconstructed.write(f.Data)
	}
	if !bytes.Equal(live, suffix) {
		t.Fatalf("live bytes changed: %q", live)
	}
	screen := awaitScreen(t, path, r.Proof, "after").Screen
	if reconstructed.terminal.String() != screen || !strings.Contains(screen, "before€") {
		t.Fatalf("replay/live boundary lost bytes: reconstructed=%q actual=%q", reconstructed.terminal.String(), screen)
	}
	_ = c.Close()
}

func TestConcurrentWriterAdmission(t *testing.T) {
	_, path := fixture(t, "ready")
	r := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, r.Proof, "ready")
	type result struct {
		connection net.Conn
		reply      Reply
		err        error
	}
	results := make(chan result, 12)
	for i := 0; i < 12; i++ {
		go func() {
			c, err := net.Dial("unix", path)
			if err != nil {
				results <- result{err: err}
				return
			}
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			err = WriteMessage(c, Request{Action: "attach"})
			var reply Reply
			if err == nil {
				err = ReadMessage(bufio.NewReader(c), &reply)
			}
			results <- result{c, reply, err}
		}()
	}
	writers := 0
	for i := 0; i < 12; i++ {
		result := <-results
		if result.connection != nil {
			defer result.connection.Close()
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.reply.OK {
			writers++
		} else if !strings.Contains(result.reply.Error, "writer conflict") {
			t.Fatalf("unexpected refusal: %+v", result.reply)
		}
	}
	if writers != 1 {
		t.Fatalf("concurrent writer count=%d", writers)
	}
}

func TestProtocolRejectsOversizeUnknownAndTrailing(t *testing.T) {
	for _, record := range []string{"{\"action\":\"status\",\"command\":\"untrusted\"}\n", "{\"action\":\"status\"} {}\n", strings.Repeat("x", MaxMessage+1) + "\n"} {
		var r Request
		if err := ReadMessage(bufio.NewReader(strings.NewReader(record)), &r); err == nil {
			t.Fatal("invalid record accepted")
		}
	}
	var out bytes.Buffer
	if err := WriteMessage(&out, Frame{Type: "output", Data: make([]byte, MaxMessage)}); err == nil || out.Len() != 0 {
		t.Fatal("oversized record partially written")
	}
	_, path := fixture(t, "ready")
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "{\"action\":\"ensure\",\"command\":\"bad\"}\n")
	var r Reply
	if err = ReadMessage(bufio.NewReader(c), &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Outcome != "refused" {
		t.Fatalf("unknown field accepted: %+v", r)
	}
	if rpc(t, path, Request{Action: "status"}).State.Running {
		t.Fatal("rejected ensure started child")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Call(ctx, path, Request{Action: "status"}); err == nil {
		t.Fatal("canceled RPC succeeded")
	}
}
