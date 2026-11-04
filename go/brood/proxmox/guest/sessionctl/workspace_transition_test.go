//go:build linux

package sessionctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

func TestWorkspaceTransitionWaitsForTeardownAndNewProof(t *testing.T) {
	for _, op := range []string{"restart", "recycle"} {
		t.Run(op, func(t *testing.T) {
			e := workspaceFixture(t)
			stopped, launched := false, false
			quietCalls, newProofs, resets := 0, 0, 0
			old := identity{PID: 123, SessionID: "old", Process: "old-start", HolderProof: "holder"}
			e.prove = func(context.Context) (identity, error) {
				if !launched {
					return old, nil
				}
				newProofs++
				switch newProofs {
				case 1:
					return identity{}, errors.New("record not ready")
				case 2:
					return old, nil
				default:
					return identity{PID: 124, SessionID: "new", Process: "new-start"}, nil
				}
			}
			e.quiet = func(context.Context) (bool, error) {
				if !stopped || launched {
					t.Fatal("quiet scan outside stop/launch boundary")
				}
				quietCalls++
				return quietCalls >= 3, nil
			}
			e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
				switch req.Action {
				case "stop":
					if stopped {
						t.Fatal("stop repeated")
					}
					stopped = true
				case "ensure":
					if launched || quietCalls < 3 {
						t.Fatal("launch before quiet or repeated")
					}
					launched = true
				}
				return liveReply(), nil
			}
			git := e.git
			e.git = func(ctx context.Context, args ...string) (string, error) {
				if args[0] == "reset" || args[0] == "clean" {
					if quietCalls < 3 {
						t.Fatal("cleanup before old processes stopped")
					}
					resets++
				}
				return git(ctx, args...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r := e.handle(ctx, workspaceRequest(op, `{"confirm":true}`))
			if !r.OK || !launched || newProofs != 3 || (op == "recycle" && resets != 2) {
				t.Fatalf("delayed transition: %+v launched=%v proofs=%d cleanup=%d", r, launched, newProofs, resets)
			}
		})
	}
}

func TestWorkspaceTransitionWaitsForNewProof(t *testing.T) {
	e := workspaceFixture(t)
	launched, proofs := false, 0
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		if req.Action == "ensure" {
			launched = true
		}
		return liveReply(), nil
	}
	e.prove = func(context.Context) (identity, error) {
		if !launched {
			return identity{PID: 123, SessionID: "old", HolderProof: "holder"}, nil
		}
		proofs++
		if proofs < 3 {
			return identity{}, errors.New("new session not ready")
		}
		return identity{PID: 124, SessionID: "new"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
	if !r.OK || proofs != 3 {
		t.Fatalf("delayed proof: %+v attempts=%d", r, proofs)
	}
}

func TestWorkspaceTransitionTimeoutAndCancellation(t *testing.T) {
	for _, phase := range []string{"stop", "proof", "unchanged-proof"} {
		for _, cancelWait := range []bool{false, true} {
			t.Run(phase+"/cancel="+strconv.FormatBool(cancelWait), func(t *testing.T) {
				e := workspaceFixture(t)
				ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
				defer cancel()
				stops, launches, attempts := 0, 0, 0
				old := identity{PID: 123, SessionID: "old", Process: "old-start", HolderProof: "holder"}
				pending := func() {
					attempts++
					if cancelWait && attempts == 2 {
						cancel()
					}
				}
				e.quiet = func(context.Context) (bool, error) {
					if phase != "stop" {
						return true, nil
					}
					pending()
					return false, nil
				}
				e.prove = func(context.Context) (identity, error) {
					if launches == 0 {
						return old, nil
					}
					pending()
					if phase == "unchanged-proof" {
						return old, nil
					}
					return identity{}, errors.New("not proven")
				}
				e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
					switch req.Action {
					case "stop":
						stops++
					case "ensure":
						launches++
					}
					return liveReply(), nil
				}
				start := time.Now()
				r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
				wantLaunches := 1
				if phase == "stop" {
					wantLaunches = 0
				}
				if r.OK || r.Outcome != "unknown" || ctx.Err() == nil || attempts < 2 || stops != 1 || launches != wantLaunches || time.Since(start) > time.Second {
					t.Fatalf("unconfirmed %s: %+v ctx=%v attempts=%d stops=%d launches=%d elapsed=%v", phase, r, ctx.Err(), attempts, stops, launches, time.Since(start))
				}
			})
		}
	}
}

func TestWorkspaceTransitionRechecksRepositoryAfterWaiting(t *testing.T) {
	e := workspaceFixture(t)
	quietCalls, launches := 0, 0
	e.quiet = func(context.Context) (bool, error) {
		quietCalls++
		if quietCalls == 1 {
			return false, nil
		}
		if quietCalls == 2 {
			if err := os.Rename(filepath.Join(e.root, ".git"), filepath.Join(e.root, "moved")); err != nil {
				t.Fatal(err)
			}
		}
		return true, nil
	}
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		if req.Action == "ensure" {
			launches++
		}
		return liveReply(), nil
	}
	r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || r.Outcome != "unknown" || quietCalls < 2 || launches != 0 || !strings.Contains(r.Text, "repository proof was lost") {
		t.Fatalf("repository changed while waiting: %+v quiet=%d launches=%d", r, quietCalls, launches)
	}
}

func TestWorkspaceTransitionInventoryErrorDoesNotLaunch(t *testing.T) {
	e := workspaceFixture(t)
	launches, scans := 0, 0
	e.quiet = func(context.Context) (bool, error) { scans++; return false, errors.New("inventory unavailable") }
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		if req.Action == "ensure" {
			launches++
		}
		return liveReply(), nil
	}
	r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || r.Outcome != "unknown" || launches != 0 || scans != 1 {
		t.Fatalf("unknown inventory: %+v launches=%d scans=%d", r, launches, scans)
	}
}
