//go:build linux

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"golang.org/x/sys/unix"
)

// autostartHelperSpec fixes executable, argv, env and credentials like the
// session helper; it starts outside the clone, as restart's tmux client does.
func autostartHelperSpec() ActorSpec {
	spec := actorSpec("/proc/self/exe")
	spec.Args = []string{"--brood-guest-autostart"}
	spec.Dir = "/workspace/home"
	return spec
}

// AutostartSession makes the Claude session exist at guest boot without a
// terminal. It retries until success or ctx ends; failures go to report and
// never stop the supervisor.
func AutostartSession(ctx context.Context, report func(error)) {
	autostartSession(ctx, launchAutostartHelper, report, autostartDelay)
}

func autostartSession(ctx context.Context, launch func(context.Context) error, report func(error), delay func(int) time.Duration) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		if launch(ctx) == nil {
			return
		}
		wait := delay(attempt)
		report(fmt.Errorf("guest Claude session autostart failed; next retry in %s", wait))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func autostartDelay(attempt int) time.Duration {
	if attempt >= 5 {
		return 30 * time.Second
	}
	return time.Second << attempt
}

// launchAutostartHelper runs the fixed UID 1000 helper once. Its output is
// actor-side and never parsed; only the exit status crosses the UID fence.
func launchAutostartHelper(parent context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("session autostart requires guest supervisor")
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	cmd, err := guestHelperCommand(autostartHelperSpec())
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return errors.New("session autostart helper unavailable")
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stop()
	if err := cmd.Wait(); err != nil {
		return errors.New("session autostart helper failed")
	}
	return nil
}

// RunSessionAutostart is the dedicated --brood-guest-autostart dispatch target.
// It accepts no arguments and starts the same fixed launch as restart.
func RunSessionAutostart(ctx context.Context) error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return errors.New("session autostart requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return errors.New("session autostart group check failed")
	}
	for _, g := range groups {
		if g != 1000 {
			return errors.New("session autostart has unexpected supplementary groups")
		}
	}
	if err := lockPrivileges(); err != nil {
		return err
	}
	unix.Umask(0077)
	return sessionctl.EnsureSession(ctx)
}
