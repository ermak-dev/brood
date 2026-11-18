package server

// BUG: Workspace reservation родителя не препятствует child enqueue и dispatch.
// HYPOTHESIS: ActorCoordinationClaims не читает top-level UID workspace-операции.
// SOLUTION: Child authority проверяет CurrentActorOperation внутри транзакции.
// RESULT: FIXED — workspace reservation исключает enqueue, start и relay delivery.
// STATUS: FIXED

import (
	"context"
	"errors"
	"sync"
	"testing"

	"brood.dev/go/brood/store"
)

func TestChildParentWorkspaceReservationBlocksEnqueue(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"restart", "wipe", "recycle", "workspace_attest"} {
		for _, confirmed := range []bool{true, false} {
			t.Run(operation+map[bool]string{true: "/inflight", false: "/unconfirmed"}[confirmed], func(t *testing.T) {
				d := newChildEngineServer(t)
				op, err := d.srv.store.AcquireActorOperation(ctx, d.master.UID, d.master.Incarnation, "parent-workspace", "operator:test", operation)
				if err != nil {
					t.Fatal(err)
				}
				if !confirmed {
					if err := d.srv.store.FinishActorOperation(ctx, op, false); err != nil {
						t.Fatal(err)
					}
				}
				c, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "one"))
				d.srv.children.dispatch(ctx)
				if !errors.Is(err, store.ErrSwarmChildDenied) || c.ID != "" {
					t.Fatalf("child accepted during parent workspace operation: %+v, %v", c, err)
				}
				if _, err := d.srv.store.SwarmChildParentAt(ctx, d.swarm, 1); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("refused enqueue left a child join: %v", err)
				}
				d.mu.Lock()
				n := len(d.messages)
				d.mu.Unlock()
				if n != 1 {
					t.Fatalf("child reached relay while parent reserved: %d deliveries", n-1)
				}
			})
		}
	}
}

func TestChildParentReservationRecheckedBeforeRelay(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"dispatch", "before-relay"} {
		t.Run(stage, func(t *testing.T) {
			d := newChildEngineServer(t)
			if _, err := d.srv.store.AcquireActorOperation(ctx, d.master.UID, d.master.Incarnation, "parent-restart", "operator:test", "restart"); err != nil {
				t.Fatal(err)
			}
			v := d.last()
			sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
			if err != nil {
				t.Fatal(err)
			}
			p := store.SwarmChildParent{Principal: childPrincipal(d.master, sw.ProjectID), Sequence: v.Sequence, Pass: v.Pass, RequestID: v.RequestID}
			// Воспроизводим сохранённую очередь прежнего сервера, который ошибочно
			// разрешил enqueue и start под workspace reservation родителя.
			legacyAllow := func(context.Context, store.SwarmChildAuthorization) error { return nil }
			c, err := d.srv.store.EnqueueSwarmChild(ctx, p, store.SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, "legacy-child", "review", "Review the patch", legacyAllow)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "dispatch" {
				d.srv.children.dispatch(ctx)
			} else {
				c, err = d.srv.store.StartSwarmChild(ctx, p, c.RequestID, c.Executor, c.Revision, legacyAllow)
				if err != nil {
					t.Fatal(err)
				}
				d.srv.children.deliver(ctx, store.SwarmChildWork{Parent: p, Task: c})
			}
			d.mu.Lock()
			n := len(d.messages)
			d.mu.Unlock()
			if n != 1 {
				t.Fatalf("reserved parent reached child relay at %s: %d child deliveries", stage, n-1)
			}
			w, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "queued"
			if stage == "before-relay" {
				want = "not_sent"
			}
			if w.Task.Delivery != want {
				t.Fatalf("unsafe replay state: %+v", w.Task)
			}
		})
	}
}

func TestChildParentWorkspaceReservationRacesScheduling(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"enqueue", "dispatch"} {
		t.Run(stage, func(t *testing.T) {
			for range 8 {
				d := newChildEngineServer(t)
				other, err := store.Open(ctx, d.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				if stage == "dispatch" {
					if _, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "one")); err != nil {
						t.Fatal(err)
					}
				}
				start := make(chan struct{})
				var workspaceErr, childErr error
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, workspaceErr = other.AcquireActorOperation(ctx, d.master.UID, d.master.Incarnation, "racing-parent-restart", "operator:test", "restart")
				}()
				go func() {
					defer wg.Done()
					<-start
					if stage == "enqueue" {
						_, childErr = d.srv.managed.createChild(ctx, d.master, childArgs(d, "one"))
					} else {
						d.srv.children.dispatch(ctx)
					}
				}()
				close(start)
				wg.Wait()
				if workspaceErr == nil && childErr == nil {
					t.Fatal("parent reservation and child scheduling both succeeded")
				}
				if workspaceErr != nil && !errors.Is(workspaceErr, store.ErrCoordinationConflict) {
					t.Fatal(workspaceErr)
				}
				if childErr != nil && !errors.Is(childErr, store.ErrSwarmChildDenied) {
					t.Fatal(childErr)
				}
				if stage == "enqueue" && workspaceErr != nil && childErr != nil {
					t.Fatal("neither scheduling contender won")
				}
				d.srv.children.dispatch(ctx)
				d.mu.Lock()
				n := len(d.messages)
				d.mu.Unlock()
				if workspaceErr == nil && n != 1 {
					t.Fatalf("child dispatched under winning parent reservation: %d deliveries", n-1)
				}
				if workspaceErr != nil && n != 2 {
					t.Fatalf("winning child was not delivered: %d deliveries", n-1)
				}
			}
		})
	}
}
