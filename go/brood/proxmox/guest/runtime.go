//go:build linux

package guest

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Dependencies is the trusted root boundary. Tests replace it without changing
// the host firewall; production callers must use SystemDependencies unchanged.
type Dependencies struct {
	EUID      func() int
	Lookup    Lookup
	Workspace func() error
	Rules     func(context.Context, string) error
	Services  func(context.Context, *Policy) (Services, error)
	Launch    func(context.Context, ActorSpec) (Process, error)
}

// SystemDependencies operates only inside a dedicated guest CT with exclusive
// actor UID 1000. Root needs CAP_NET_ADMIN, CAP_SETUID, CAP_SETGID, CAP_CHOWN,
// CAP_DAC_OVERRIDE, CAP_FOWNER and CAP_KILL, normal /proc access and pidfd support.
// The guest network namespace must support inet nftables NAT/filter/conntrack,
// including IPv6 loopback. The parent must complete DHCP before Start and leave
// the normal DHCP resolver in resolv.conf; pointing it at this DNS service would
// recurse. These dependencies never change DHCP, resolv.conf or host policy.
// The trusted root relay may reach its configured private-LAN public origin;
// that origin must not be added to the actor policy as a private-address bypass.
func SystemDependencies() *Dependencies {
	return &Dependencies{
		EUID: os.Geteuid,
		Lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		Workspace: func() error { return prepareWorkspace("/workspace", 1000, 1000) },
		Rules:     systemRules, Services: startServices, Launch: launchActor,
	}
}

// Config carries no relay or PVE credentials. Hosts is an exact public-host
// allowlist, not a URL list. Executable defaults to the running supervisor.
type Config struct {
	Hosts        []string
	Executable   string
	Dependencies *Dependencies
}

// Runtime owns the actor and root enforcement services. Start means the child
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

// Close stops the whole actor before closing root services or removing rules.
func (r *Runtime) Close() error { r.once.Do(func() { close(r.stop) }); return r.Wait() }

// Start refuses missing dependencies and installs enforcement before any child.
// An existing brood_guest nft table is an error, never overwritten or adopted.
func Start(ctx context.Context, c Config) (*Runtime, error) {
	d := c.Dependencies
	if d == nil || d.EUID == nil || d.Lookup == nil || d.Workspace == nil || d.Rules == nil || d.Services == nil || d.Launch == nil {
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
	policy, err := NewPolicy(c.Hosts, d.Lookup)
	if err != nil {
		return nil, err
	}
	if err := d.Workspace(); err != nil {
		return nil, err
	}
	if err := d.Rules(ctx, nftRules()); err != nil {
		return nil, err
	}
	cleanupRules := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return d.Rules(cleanupCtx, "delete table inet brood_guest\n")
	}
	serviceCtx, cancel := context.WithCancel(context.Background())
	services, err := d.Services(serviceCtx, policy)
	if err != nil || services == nil {
		cancel()
		if services != nil {
			err = errors.Join(err, services.Close())
		}
		return nil, errors.Join(errors.New("guest enforcement services failed"), err, cleanupRules())
	}
	fail := func(err error) (*Runtime, error) {
		cancel()
		return nil, errors.Join(err, services.Close(), cleanupRules())
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	select {
	case err := <-services.Done():
		return fail(errors.Join(errors.New("guest enforcement service stopped"), err))
	default:
	}
	process, err := d.Launch(ctx, actorSpec(c.Executable))
	if err != nil || process == nil {
		startErr := errors.Join(errors.New("guest actor start failed"), err)
		if process != nil {
			if closeErr := process.Close(); closeErr != nil {
				cancel()
				return nil, errors.Join(startErr, closeErr, services.Close())
			}
			startErr = errors.Join(startErr, process.Wait())
		}
		return fail(startErr)
	}
	r := &Runtime{done: make(chan struct{}), stop: make(chan struct{})}
	childDone := make(chan error, 1)
	go func() { childDone <- process.Wait() }()
	go func() {
		defer close(r.done)
		childExited := false
		select {
		case err := <-childDone:
			r.err = err
			childExited = true
		case err := <-services.Done():
			r.err = errors.Join(errors.New("guest enforcement service stopped"), err)
		case <-ctx.Done():
			r.err = ctx.Err()
		case <-r.stop:
		}
		if err := process.Close(); err != nil {
			// Retain enforcement if descendants cannot be proven dead.
			cancel()
			r.err = errors.Join(r.err, err, services.Close())
			return
		}
		if !childExited {
			// SIGKILL is expected after an intentional stop, not a new failure.
			<-childDone
		}
		cancel()
		r.err = errors.Join(r.err, services.Close(), cleanupRules())
	}()
	return r, nil
}
