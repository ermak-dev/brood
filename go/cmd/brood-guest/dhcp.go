//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Обёртка не принимает команды или настройки сети от актора. Каталог 0700,
// hook 0700; udhcpc передаёт только lease доверенному image hook.
const dhcpHook = `#!/bin/sh
set -eu
state=${0%/*}
fail() { : > "$state/failed"; exit 1; }
if [ "${1:-}" = deconfig ] && [ -f "$state/ready" ]; then fail; fi
/usr/local/libexec/proxmox-guest-dhcp.sh "$@" || fail
case "${1:-}" in bound|renew) : > "$state/ready";; esac
`

func helperEnv() []string {
	return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C"}
}
func dhcpCommand(ctx context.Context, hook string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/busybox", "udhcpc", "-f", "-n", "-i", "eth0", "-s", hook, "-t", "5", "-T", "3")
	cmd.Env = helperEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

type dhcpService struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
	err  error
}

func (s *dhcpService) Wait() error { <-s.done; return s.err }
func (s *dhcpService) kill() {
	s.once.Do(func() { _ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL) })
}
func (s *dhcpService) Close() error {
	s.kill()
	<-s.done
	return nil
}
func marker(dir, name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
func startDHCP(ctx context.Context) (service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("/run", "brood-dhcp-")
	if err != nil {
		return nil, errors.New("cannot create guest DHCP state")
	}
	return runDHCP(ctx, dir, dhcpCommand, 45*time.Second)
}

func runDHCP(ctx context.Context, dir string, command func(context.Context, string) *exec.Cmd, leaseTimeout time.Duration) (service, error) {
	hook := filepath.Join(dir, "hook")
	if err := os.WriteFile(hook, []byte(dhcpHook), 0700); err != nil {
		os.RemoveAll(dir)
		return nil, errors.New("cannot prepare guest DHCP hook")
	}
	cmd := command(ctx, hook)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, errors.New("cannot start guest DHCP client")
	}
	s := &dhcpService{cmd: cmd, done: make(chan struct{})}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	ready := make(chan struct{})
	go func() {
		defer close(s.done)
		defer os.RemoveAll(dir)
		defer s.kill()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(leaseTimeout)
		defer deadline.Stop()
		bound := false
		for {
			select {
			case <-exited:
				s.err = errors.New("guest DHCP client exited")
				return
			case <-ctx.Done():
				s.err = ctx.Err()
			case <-deadline.C:
				s.err = errors.New("guest DHCP lease acquisition timed out")
			case <-ticker.C:
				if marker(dir, "failed") {
					s.err = errors.New("guest DHCP lease hook failed")
				} else if !bound && marker(dir, "ready") {
					bound = true
					deadline.Stop()
					close(ready)
				}
			}
			if s.err != nil {
				s.kill()
				<-exited
				return
			}
		}
	}()
	select {
	case <-ready:
		return s, nil
	case <-s.done:
		return nil, s.err
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	}
}
