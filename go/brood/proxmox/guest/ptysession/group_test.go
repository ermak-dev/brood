//go:build linux

package ptysession

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// FEATURE: Подтверждение выхода process group до освобождения PID лидера.
// SPEC: Зомби не исполняются; живой участник группы блокирует завершение.
func TestLiveProcessGroup(t *testing.T) {
	for _, tt := range []struct {
		name, stat    string
		group         int
		live, invalid bool
	}{
		{"live", "21 (worker (job)) S 1 17 17", 17, true, false},
		{"other group", "21 (worker) S 1 18 18", 17, false, false},
		{"zombie", "21 (worker) Z 1 17 17", 17, false, false},
		{"dead", "21 (worker) X 1 17 17", 17, false, false},
		{"invalid", "21 worker S 1 17", 17, false, true},
		{"short", "21 (worker) S 1", 17, false, true},
		{"invalid group", "21 (worker) S 1 bad 17", 17, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			live, err := liveProcessGroup([]byte(tt.stat), tt.group)
			if (err != nil) != tt.invalid || live != tt.live {
				t.Fatalf("live=%v err=%v", live, err)
			}
		})
	}
}

// BUG #237: обход /proc читает stat каждого процесса машины. Посторонний
// процесс, которого родитель забрал между open и read его stat, давал ESRCH,
// и Close под нагрузкой CI возвращал unknown при уже завершённой группе.
// SPEC: Исчезнувший процесс не участник группы; прочий отказ чтения - unknown.
func TestFinishGroupIgnoresProcessReapedDuringScan(t *testing.T) {
	leader := startSleeper(t, true)
	bystander := startSleeper(t, false)
	target := statPath(bystander.Process.Pid)
	var reaped error
	raced := false
	read := func(path string) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if path == target && !raced {
			// Тот же порядок, что у os.ReadFile, но reap попадает между open и read.
			raced = true
			_ = bystander.Process.Kill()
			_ = bystander.Wait()
			_, reaped = io.ReadAll(f)
			return nil, reaped
		}
		return io.ReadAll(f)
	}
	err := finishGroupWith(leader.Process.Pid, read)
	if !errors.Is(reaped, syscall.ESRCH) {
		t.Fatalf("bystander was not reaped between open and read: raced=%v err=%v", raced, reaped)
	}
	if err != nil {
		t.Fatalf("process reaped outside the group left cleanup unconfirmed: %v", err)
	}
}

func TestFinishGroupRefusesUnreadableProcess(t *testing.T) {
	leader := startSleeper(t, true)
	target := statPath(os.Getpid())
	read := func(path string) ([]byte, error) {
		if path == target {
			return nil, &fs.PathError{Op: "read", Path: path, Err: syscall.EACCES}
		}
		return os.ReadFile(path)
	}
	err := finishGroupWith(leader.Process.Pid, read)
	if err == nil || !strings.Contains(err.Error(), "membership is unconfirmed") {
		t.Fatalf("unreadable process accepted as outside the group: %v", err)
	}
}

// startSleeper запускает долгий процесс; ownGroup делает его лидером новой
// группы. Лидер после SIGKILL остаётся зомби до Cleanup, как PID, закреплённый
// WNOWAIT в reap.
func startSleeper(t *testing.T, ownGroup bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: ownGroup}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func statPath(pid int) string { return "/proc/" + strconv.Itoa(pid) + "/stat" }
