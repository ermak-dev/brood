//go:build linux

package sessionctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"golang.org/x/sys/unix"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// openSafe rejects symlinks in every component, including the supplied root.
func openSafe(path string, directory bool) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("unsafe file path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if path == "/" && directory {
		return os.NewFile(uintptr(fd), path), nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			unix.Close(fd)
			return nil, errors.New("unsafe file path")
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 || directory {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, p, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil || (directory && !st.IsDir()) || (!directory && !st.Mode().IsRegular()) {
		f.Close()
		return nil, errors.New("unsafe file type")
	}
	return f, nil
}
func readSafe(path string, limit int64) ([]byte, error) {
	f, err := openSafe(path, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(data) > int(limit) {
		return nil, errors.New("file exceeds size limit")
	}
	return data, err
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

type process struct {
	start      string
	parent     int
	group      string
	foreground string
	tty        string
	cwd        string
}

func processInfo(pid int) (process, error) {
	if pid < 1 {
		return process{}, errors.New("invalid process")
	}
	root := "/proc/" + strconv.Itoa(pid)
	readStart := func() (process, error) {
		data, err := os.ReadFile(root + "/stat")
		if err != nil || len(data) > 8192 {
			return process{}, errors.New("process stat unavailable")
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return process{}, errors.New("invalid process stat")
		}
		f := strings.Fields(string(data[end+1:]))
		if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
			return process{}, errors.New("process is not live")
		}
		parent, err := strconv.Atoi(f[1])
		if err != nil {
			return process{}, err
		}
		return process{start: f[19], parent: parent, group: f[2], foreground: f[5], tty: f[4]}, nil
	}
	before, err := readStart()
	if err != nil {
		return process{}, err
	}
	st, err := os.Stat(root)
	if err != nil || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return process{}, errors.New("process owner mismatch")
	}
	before.cwd, err = os.Readlink(root + "/cwd")
	if err != nil {
		return process{}, err
	}
	after, err := readStart()
	if err != nil || after.start != before.start {
		return process{}, errors.New("process changed")
	}
	return before, nil
}

func proveLive(ctx context.Context) (identity, error) {
	return proveIdentity(ctx, fixedConfig, "/workspace/project", callPTY, true)
}
func proveIdentity(ctx context.Context, config, cwd string, call ptyCall, requirePTY bool) (identity, error) {
	dir, err := openSafe(filepath.Join(config, "sessions"), true)
	if err != nil {
		return identity{}, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(257)
	if err != nil && err != io.EOF {
		return identity{}, err
	}
	if len(entries) > 256 {
		return identity{}, errors.New("too many session records")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return identity{}, err
	}
	var hits []identity
	for _, entry := range entries {
		if ctx.Err() != nil {
			return identity{}, ctx.Err()
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := readSafe(filepath.Join(config, "sessions", entry.Name()), 16*1024)
		if err != nil {
			continue
		}
		var s struct {
			PID    int    `json:"pid"`
			ID     string `json:"sessionId"`
			CWD    string `json:"cwd"`
			Socket string `json:"messagingSocketPath"`
		}
		if json.Unmarshal(data, &s) != nil || s.CWD != cwd || !safeID.MatchString(s.ID) || s.PID < 1 || !filepath.IsAbs(s.Socket) {
			continue
		}
		before, err := os.Lstat(s.Socket)
		if err != nil {
			continue
		}
		if before.Mode()&os.ModeSocket == 0 {
			return identity{}, errors.New("invalid session socket")
		}
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", s.Socket)
		if err != nil {
			continue
		}
		raw, err := conn.(*net.UnixConn).SyscallConn()
		var cred *unix.Ucred
		if err == nil {
			err = raw.Control(func(fd uintptr) { cred, _ = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
		}
		conn.Close()
		if err != nil || cred == nil || cred.Pid != int32(s.PID) || cred.Uid != uint32(os.Getuid()) {
			return identity{}, errors.New("session socket peer mismatch")
		}
		after, err := os.Lstat(s.Socket)
		if err != nil || !os.SameFile(before, after) {
			return identity{}, errors.New("session socket changed")
		}
		proc, err := processInfo(s.PID)
		if err != nil || proc.cwd != cwd {
			return identity{}, errors.New("session process mismatch")
		}
		env, err := os.Open("/proc/" + strconv.Itoa(s.PID) + "/environ")
		if err != nil {
			return identity{}, err
		}
		environment, err := io.ReadAll(io.LimitReader(env, 1024*1024+1))
		env.Close()
		if err != nil || len(environment) > 1024*1024 {
			return identity{}, errors.New("process environment unavailable")
		}
		home, configured := "", ""
		for _, item := range strings.Split(string(environment), "\x00") {
			if strings.HasPrefix(item, "HOME=") {
				home = strings.TrimPrefix(item, "HOME=")
			}
			if strings.HasPrefix(item, "CLAUDE_CONFIG_DIR=") {
				configured = strings.TrimPrefix(item, "CLAUDE_CONFIG_DIR=")
			}
		}
		if configured == "" {
			configured = filepath.Join(home, ".claude")
		}
		if configured != config {
			return identity{}, errors.New("session config home mismatch")
		}
		real, err := filepath.EvalSymlinks(s.Socket)
		if err != nil {
			return identity{}, err
		}
		key, err := readSafe(filepath.Join(config, "sessions", fmt.Sprintf("%d.%s.key", s.PID, digest([]byte(real)))), 4096)
		if err != nil {
			return identity{}, errors.New("session process-start key unavailable")
		}
		var k struct {
			Start string `json:"procStart"`
		}
		if json.Unmarshal(key, &k) != nil || k.Start != proc.start {
			return identity{}, errors.New("session process-start key mismatch")
		}
		var holder ptysession.Reply
		if requirePTY {
			holder, err = call(ctx, ptysession.Request{Action: "status"})
			if err != nil || !holder.OK || !holder.State.Running || holder.Proof == "" || holder.State.Epoch == "" || holder.State.PID != s.PID || holder.State.Start != proc.start {
				return identity{}, errors.New("Claude process does not match the current PTY holder")
			}
			if proc.tty == "0" || proc.group != proc.foreground {
				return identity{}, errors.New("Claude session is not foreground on its PTY")
			}
			current, err := call(ctx, ptysession.Request{Action: "status"})
			if err != nil || !current.OK || current.State != holder.State || current.Proof != holder.Proof {
				return identity{}, errors.New("PTY holder changed during identity proof")
			}
		}
		final, err := processInfo(s.PID)
		if err != nil || final != proc {
			return identity{}, errors.New("session process changed")
		}
		stat := after.Sys().(*syscall.Stat_t)
		hits = append(hits, identity{SessionID: s.ID, Process: strings.TrimSpace(string(boot)) + ":" + proc.start, PID: s.PID, Socket: s.Socket, Device: uint64(stat.Dev), Inode: stat.Ino, HolderProof: holder.Proof, Epoch: holder.State.Epoch})
	}
	if len(hits) != 1 {
		return identity{}, errors.New("no unique proven live Claude session")
	}
	return hits[0], nil
}
