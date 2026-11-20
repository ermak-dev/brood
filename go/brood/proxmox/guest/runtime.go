//go:build linux

package guest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	return systemDependencies("/workspace", 1000, 1000, systemStepPolicy())
}

// systemDependencies is SystemDependencies with the workspace, its owner and
// the step policy paths that tests substitute.
func systemDependencies(workspace string, uid, gid int, step stepPolicy) *Dependencies {
	return &Dependencies{
		EUID: os.Geteuid,
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
		Launch: launchActor,
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
	done    chan struct{}
	stop    chan struct{}
	quiesce chan quiesceRequest
	once    sync.Once
	err     error

	stepOperation sync.Mutex // Serializes workspace attestation and explicit session replacement.
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

var errWorkspaceRequestRevoked = errors.New("workspace attestation canceled before quiesce; session unchanged")

func (r *Runtime) transition(ctx context.Context, fn func(context.Context) error, step *stepLaunchRequest) error {
	q := quiesceRequest{ctx: ctx, run: fn, reply: make(chan error, 1), step: step}
	var requestDone, authorityDone <-chan struct{}
	if step != nil {
		requestDone, authorityDone = step.request.Done(), step.authority.Done()
	}
	select {
	case r.quiesce <- q:
	case <-r.done:
		return errors.New("guest runtime stopped")
	case <-ctx.Done():
		return ctx.Err()
	case <-requestDone:
		return errWorkspaceRequestRevoked
	case <-authorityDone:
		return errWorkspaceRequestRevoked
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

// Close stops the whole actor and waits for the ordered cleanup.
func (r *Runtime) Close() error {
	r.once.Do(func() {
		close(r.stop)
		r.revokeStepLaunch(nil)
	})
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
					r.err = errors.Join(errors.New("revoked inbox holder could not be proven stopped"), err)
					return
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
				if q.step != nil && (q.step.request.Err() != nil || q.step.authority.Err() != nil) {
					q.reply <- errWorkspaceRequestRevoked
					continue
				}
				heldLaunch, heldRevoked = nil, nil
				r.revokeStepLaunch(nil)
				if err := process.Close(); err != nil {
					// Как при остановке: недоказанная смерть потомков завершает runtime.
					q.reply <- errors.New("actor processes could not be proven stopped; workspace not inspected")
					r.err = errors.Join(errors.New("guest actor stop for quiesce failed"), err)
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
				if errors.Is(runErr, errStepSecretPublication) {
					// A failed atomic publication can leave the new projection on
					// disk. Do not start even an ordinary holder on uncertain state.
					q.reply <- runErr
					r.err = runErr
					childExited, running = true, false
					continue
				}
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
						r.err = stopErr
						return
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
			r.err = errors.Join(r.err, err)
			return
		}
		if !childExited {
			// SIGKILL is expected after an intentional stop, not a new failure.
			<-childDone
		}
		r.err = errors.Join(r.err, clearInboxPolicy())
	}()
	return r, nil
}
