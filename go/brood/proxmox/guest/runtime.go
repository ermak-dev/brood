//go:build linux

package guest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"brood.dev/go/brood/proxmox/egress"
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
	return systemDependencies("/workspace", 1000, 1000, systemStepPolicy())
}

// systemDependencies is SystemDependencies with the workspace, its owner and
// the step policy paths that tests substitute.
func systemDependencies(workspace string, uid, gid int, step stepPolicy) *Dependencies {
	return &Dependencies{
		EUID: os.Geteuid,
		Lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		Workspace: func() error {
			if err := prepareWorkspace(workspace, uid, gid); err != nil {
				return err
			}
			// Новый runtime начинает со свободной сессии (#224): /etc и, без
			// tmpfs, /run переживают перезапуск CT вместе с режимом прошлого шага.
			if err := step.leave(); err != nil {
				return fmt.Errorf("step session policy of the previous start cannot be cleared: %w", err)
			}
			return nil
		},
		Rules: systemRules, Services: startServices, Launch: launchActor,
	}
}

// Config carries no relay or PVE credentials. Hosts is the legacy public-host
// allowlist and cannot be combined with Network. A nonnil Network may be offline.
// Executable defaults to the running supervisor.
type Config struct {
	Network      *egress.Policy
	Hosts        []string
	Executable   string
	Dependencies *Dependencies
}

// Runtime owns the actor and root enforcement services. Start means the child
// was launched, not that ttyd is ready; the parent may probe TerminalDialTarget.
type Runtime struct {
	done    chan struct{}
	stop    chan struct{}
	quiesce chan quiesceRequest
	once    sync.Once
	err     error

	stepOperation sync.Mutex // Serializes opted-in attestation and explicit session replacement.
	stepMu        sync.Mutex
	stepLaunch    *stepLaunch
	inboxPolicy   *stepInboxSessionPolicy // Installed before advertising the prepared inbox.
	heldInboxApp  atomic.Bool             // Remains true until the holder is physically retired.
}

type stepInboxSessionPolicy struct {
	enter     func() error
	authorize func(inboxProcessIdentity) error
	leave     func() error
}

func (r *Runtime) stepInboxPolicy() *stepInboxSessionPolicy {
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	return r.inboxPolicy
}

type quiesceRequest struct {
	ctx   context.Context
	run   func(context.Context) error
	reply chan error
	step  *stepLaunchRequest
}

// Quiesce stops every actor process, runs fn while none exists and relaunches
// the fixed actor. UID 1000 is exclusive to the actor, so the same sweep as at
// shutdown proves the window: no model process can write the workspace or
// trace the fixed helper that fn starts. An unproven stop or a failed relaunch
// ends the runtime, as a child exit would.
func (r *Runtime) Quiesce(ctx context.Context, fn func(context.Context) error) error {
	return r.transition(ctx, fn, nil)
}

func (r *Runtime) transition(ctx context.Context, fn func(context.Context) error, step *stepLaunchRequest) error {
	q := quiesceRequest{ctx: ctx, run: fn, reply: make(chan error, 1), step: step}
	select {
	case r.quiesce <- q:
	case <-r.done:
		return errors.New("guest runtime stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-q.reply
}

func (r *Runtime) stopping() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// runQuiet runs fn with a context that also ends with the runtime.
func (r *Runtime) runQuiet(runtime context.Context, q quiesceRequest) error {
	ctx, cancel := context.WithCancel(q.ctx)
	defer cancel()
	stop := context.AfterFunc(runtime, cancel)
	defer stop()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-finished:
		}
	}()
	return q.run(ctx)
}

// TerminalDialTarget is deliberately not configurable by actor or guest env.
func (*Runtime) TerminalDialTarget() string { return "127.0.0.1:8080" }

// Wait returns after child termination and ordered cleanup, including on failure.
func (r *Runtime) Wait() error { <-r.done; return r.err }

// Close stops the whole actor before closing root services or removing rules.
func (r *Runtime) Close() error {
	r.once.Do(func() {
		close(r.stop)
		r.revokeStepLaunch(nil)
	})
	return r.Wait()
}

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
	var policy *Policy
	var err error
	if c.Network != nil {
		if len(c.Hosts) != 0 {
			return nil, errors.New("legacy hosts and selected network policy cannot be combined")
		}
		policy, err = NewNetworkPolicy(c.Network.Rules(), d.Lookup)
	} else {
		policy, err = NewPolicy(c.Hosts, d.Lookup)
	}
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
	spec := actorSpec(c.Executable)
	process, err := d.Launch(ctx, spec)
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
	r := &Runtime{done: make(chan struct{}), stop: make(chan struct{}), quiesce: make(chan quiesceRequest)}
	wait := func(p Process, launch *stepLaunch) chan error {
		done := make(chan error, 1)
		go func() {
			err := p.Wait()
			done <- err
			if launch != nil {
				r.revokeStepLaunch(launch)
			}
		}()
		return done
	}
	childDone := wait(process, nil)
	go func() {
		defer close(r.done)
		childExited := false
		var heldLaunch *stepLaunch
		var heldRevoked <-chan struct{}
		var activeInboxPolicy *stepInboxSessionPolicy
		clearInboxPolicy := func() error {
			if activeInboxPolicy == nil {
				return nil
			}
			if err := activeInboxPolicy.leave(); err != nil {
				return errors.New("guest inbox policy could not be restored")
			}
			activeInboxPolicy = nil
			return nil
		}
		for running := true; running; {
			select {
			case err := <-childDone:
				r.err = err
				childExited = true
				running = false
			case err := <-services.Done():
				r.err = errors.Join(errors.New("guest enforcement service stopped"), err)
				running = false
			case <-ctx.Done():
				r.err = ctx.Err()
				running = false
			case <-r.stop:
				running = false
			case <-heldRevoked:
				// An exited holder ends the runtime as before. Its waiter publishes
				// the exit before canceling launch authority.
				select {
				case r.err = <-childDone:
					childExited, running = true, false
					continue
				default:
				}
				r.revokeStepLaunch(heldLaunch)
				heldLaunch, heldRevoked = nil, nil
				if err := process.Close(); err != nil {
					cancel()
					r.err = errors.Join(errors.New("revoked inbox holder could not be proven stopped"), err, services.Close())
					return // Retain enforcement while descendants may still exist.
				}
				<-childDone
				if err := clearInboxPolicy(); err != nil || ctx.Err() != nil || r.stopping() {
					r.err = errors.Join(err, ctx.Err())
					childExited, running = true, false
					continue
				}
				next, err := d.Launch(ctx, spec)
				if next != nil {
					process, childDone = next, wait(next, nil)
				}
				if err != nil || next == nil {
					r.err = errors.Join(errors.New("ordinary holder could not be restored after inbox revocation"), err)
					childExited, running = next == nil, false
					continue
				}
				r.heldInboxApp.Store(false)
			case q := <-r.quiesce:
				heldLaunch, heldRevoked = nil, nil
				r.revokeStepLaunch(nil)
				if err := process.Close(); err != nil {
					// Как при остановке: недоказанная смерть потомков оставляет enforcement.
					q.reply <- errors.New("actor processes could not be proven stopped; workspace not inspected")
					cancel()
					r.err = errors.Join(errors.New("guest actor stop for quiesce failed"), err, services.Close())
					return
				}
				// Код выхода убитого актора — ожидаемое следствие, не отказ.
				<-childDone
				if err := clearInboxPolicy(); err != nil {
					q.reply <- err
					r.err = err
					childExited, running = true, false
					continue
				}
				r.heldInboxApp.Store(false)
				runErr := r.runQuiet(ctx, q)
				if ctx.Err() != nil || r.stopping() {
					q.reply <- errors.Join(errors.New("guest runtime stopped during quiesce"), runErr)
					r.err = ctx.Err()
					childExited, running = true, false
					continue
				}
				applied := runErr == nil && q.step != nil && q.step.applied()
				nextSpec := spec
				nextSpec.holdPTY = applied && q.step.inbox
				if nextSpec.ptyStartHeld() {
					if policy := r.stepInboxPolicy(); policy != nil {
						// A failed publication may have renamed the drop-in before
						// failing to sync it. Restore the baseline before any app.
						activeInboxPolicy = policy
						if err := policy.enter(); err != nil {
							refusal := errors.New("guest inbox policy could not be prepared")
							if errors.Is(err, errInboxScaffold) {
								refusal = errInboxScaffold
							}
							q.reply <- refusal
							r.err = refusal
							childExited, running = true, false
							continue
						}
					}
				}
				next, err := d.Launch(ctx, nextSpec)
				if next != nil {
					var launch *stepLaunch
					if err == nil && applied {
						if nextSpec.ptyStartHeld() {
							launch, err = r.recordHeldStepLaunch(ctx, q.step, next)
						} else if ctx.Err() == nil {
							launch = r.recordStepLaunch(q.step, next)
						}
						q.step.launch = launch
					}
					process, childDone = next, wait(next, launch)
					if launch != nil && nextSpec.ptyStartHeld() {
						heldLaunch, heldRevoked = launch, launch.ctx.Done()
						r.heldInboxApp.Store(true)
					}
				}
				if next != nil && errors.Is(err, errHeldStepLaunchRevoked) && ctx.Err() == nil && !r.stopping() {
					// The exact attempt is over, not the runtime. Prove the held
					// actor stopped before restoring a holder without an inbox launch.
					if stopErr := process.Close(); stopErr != nil {
						q.reply <- errors.New("revoked inbox actor could not be proven stopped")
						cancel()
						r.err = errors.Join(stopErr, services.Close())
						return // Retain enforcement while descendants may still exist.
					}
					<-childDone
					if err := clearInboxPolicy(); err != nil {
						q.reply <- err
						r.err = err
						childExited, running = true, false
						continue
					}
					next = nil
					if ctx.Err() == nil && !r.stopping() {
						next, err = d.Launch(ctx, spec)
					}
					if next != nil {
						process, childDone = next, wait(next, nil)
					}
					if err == nil && next != nil {
						q.reply <- errHeldStepLaunchRevoked
						continue
					}
				}
				if err != nil || next == nil {
					q.reply <- errors.Join(errors.New("guest actor relaunch failed"), runErr)
					r.err = errors.Join(errors.New("guest actor relaunch failed"), err)
					childExited, running = next == nil, false
					continue
				}
				q.reply <- runErr
			}
		}
		r.revokeStepLaunch(nil)
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
		r.err = errors.Join(r.err, clearInboxPolicy(), services.Close(), cleanupRules())
	}()
	return r, nil
}
