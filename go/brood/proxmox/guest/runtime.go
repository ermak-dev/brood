//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Dependencies is the trusted root boundary. Tests replace it without touching
// the host; production callers must use SystemDependencies unchanged.
type Dependencies struct {
	EUID      func() int
	Workspace func() error
	Launch    func(context.Context, ActorSpec) (Process, error)
}

// SystemDependencies operates only inside a dedicated guest CT with exclusive
// actor UID 1000. Root needs CAP_SETUID, CAP_SETGID, CAP_CHOWN,
// CAP_DAC_OVERRIDE, CAP_FOWNER and CAP_KILL, normal /proc access and pidfd
// support. The network belongs to the distribution: these dependencies never
// configure addresses, the resolver or a firewall, and the actor uses the
// container network without restriction.
func SystemDependencies() *Dependencies {
	return &Dependencies{
		EUID:      os.Geteuid,
		Workspace: func() error { return prepareWorkspace("/workspace", 1000, 1000) },
		Launch:    launchActor,
	}
}

// Config carries no relay or PVE credentials. Executable defaults to the
// running supervisor.
type Config struct {
	Executable   string
	Dependencies *Dependencies
}

// Runtime owns the actor process tree. Start means the child
// was launched, not that ttyd is ready; the parent may probe TerminalDialTarget.
type Runtime struct {
	done chan struct{}
	stop chan struct{}
	once sync.Once
	err  error
}

// TerminalDialTarget is deliberately not configurable by actor or guest env.
func (*Runtime) TerminalDialTarget() string { return "127.0.0.1:8080" }

// Wait returns after child termination and ordered cleanup, including on failure.
func (r *Runtime) Wait() error { <-r.done; return r.err }

// Close stops the whole actor and waits for the ordered cleanup.
func (r *Runtime) Close() error {
	r.once.Do(func() { close(r.stop) })
	return r.Wait()
}

// Start refuses missing dependencies and prepares the workspace before any child.
func Start(ctx context.Context, c Config) (*Runtime, error) {
	d := c.Dependencies
	if d == nil || d.EUID == nil || d.Workspace == nil || d.Launch == nil {
		return nil, errors.New("guest supervisor dependencies are required")
	}
	if d.EUID() != 0 {
		return nil, errors.New("guest supervisor must run as root")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.Executable == "" {
		c.Executable = "/proc/self/exe"
	}
	if !filepath.IsAbs(c.Executable) {
		return nil, errors.New("guest executable must be absolute")
	}
	if err := d.Workspace(); err != nil {
		return nil, err
	}
	spec := actorSpec(c.Executable)
	process, err := d.Launch(ctx, spec)
	if err != nil || process == nil {
		startErr := errors.Join(errors.New("guest actor start failed"), err)
		if process != nil {
			if closeErr := process.Close(); closeErr != nil {
				return nil, errors.Join(startErr, closeErr)
			}
			startErr = errors.Join(startErr, process.Wait())
		}
		return nil, startErr
	}
	r := &Runtime{done: make(chan struct{}), stop: make(chan struct{})}
	childDone := make(chan error, 1)
	go func() { childDone <- process.Wait() }()
	go func() {
		defer close(r.done)
		childExited := false
		select {
		case r.err = <-childDone:
			childExited = true
		case <-ctx.Done():
			r.err = ctx.Err()
		case <-r.stop:
		}
		if err := process.Close(); err != nil {
			r.err = errors.Join(r.err, err)
			return
		}
		if !childExited {
			// SIGKILL is expected after an intentional stop, not a new failure.
			<-childDone
		}
	}()
	return r, nil
}
