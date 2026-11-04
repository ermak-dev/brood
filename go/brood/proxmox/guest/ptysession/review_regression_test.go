//go:build linux

package ptysession

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// BUG: Выход лидера оставлял живых потомков той же process group (#162).
// SPEC: Перед stopped/reap надо завершить всю закреплённую группу, иначе
// ensure может запустить второй сеанс поверх незавершённой работы.
func TestNaturalExitDescendantFixture(t *testing.T) {
	if os.Getenv("BROOD_NATURAL_EXIT_FIXTURE") != "1" {
		return
	}
	signal.Ignore(syscall.SIGHUP)
	child := exec.Command("/bin/sleep", "60")
	if child.Start() != nil {
		os.Exit(31)
	}
	if os.WriteFile(os.Getenv("CHILD_PID_PATH"), []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
		os.Exit(32)
	}
	fmt.Print("DESCENDANT162_READY")
	var trigger [1]byte
	_, _ = os.Stdin.Read(trigger[:])
	os.Exit(0)
}

func TestNaturalExitCleansOwnedProcessGroup(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	file := filepath.Join(root, "child.pid")
	host, err := New(Config{Command: []string{exe, "-test.run=^TestNaturalExitDescendantFixture$"}, Env: []string{"BROOD_NATURAL_EXIT_FIXTURE=1", "CHILD_PID_PATH=" + file}, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "pty.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer host.Close()
	go func() { _ = host.Serve(ctx, listener) }()
	state := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, state.Proof, "DESCENDANT162_READY")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	defer func() { _ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0) }()
	host.mu.Lock()
	exited := host.current.done
	host.mu.Unlock()
	mustOK(t, rpc(t, path, Request{Action: "input", Proof: state.Proof, Data: []byte("x\n")}))
	select {
	case <-exited:
	case <-time.After(4 * time.Second):
		t.Fatal("natural exit not published")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		end := strings.LastIndexByte(string(stat), ')')
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) > 0 && fields[0] != "Z" && fields[0] != "X" {
			t.Fatalf("owned descendant remains live after stopped: pid=%d state=%s", pid, fields[0])
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
}

// BUG: DECOM после cursor restore возвращал реальный терминал к началу region.
// SPEC: Origin mode восстанавливается до последнего CUP; CUP при DECOM —
// относительно top margin. Числа сверены с независимым libvterm, не тем же VT.
func TestReplayOriginModePrecedesRelativeCursor(t *testing.T) {
	observer := newObserver(80, 24)
	defer observer.close()
	observer.write([]byte("\x1b[2;20r\x1b[?6h\x1b[3;4Htext"))
	replay, err := observer.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	mode := strings.LastIndex(string(replay), "\x1b[?6h")
	cursor := strings.LastIndex(string(replay), "\x1b[3;8H")
	if mode < 0 || cursor < mode {
		t.Fatalf("origin-relative cursor missing after final DECOM: %q", replay)
	}
}
