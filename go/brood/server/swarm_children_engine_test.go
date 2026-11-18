package server

// FEATURE: Execute durable role-addressed children on existing actors (#289).
// SPEC: Commit one allocation before relay delivery, retain uncertain effects,
// cancel only unsent work, compute joins without early parent progression, and
// recheck current identity, namespace, delegation and role inside SQLite writes.
// IMPLEMENTATION: Bounded dispatcher and authenticated actor service over SQLite.
// RESULT: IMPLEMENTED — full Go suite and focused race checks pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func newChildEngineServer(t *testing.T) *dispatchServer {
	t.Helper()
	d := newDispatchServer(t)
	// Manual scheduling makes delivery interleavings deterministic in this fixture.
	d.srv.children.close()
	d.srv.children = newChildDispatcher(d.srv)
	d.bind("build", d.master.UID)
	d.bind("review", d.worker.UID)
	d.start("deliver")
	return d
}

func childArgs(d *dispatchServer, key string) childCreateArguments {
	return childCreateArguments{Sequence: 1, ParentRequestID: d.committed().Request, RequestID: key, Role: "review", Task: "Implement " + key, Mode: "all", Total: 2}
}

func TestChildExecutionEngineDeliveryAndJoin(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	m := d.srv.managed
	args := childArgs(d, "one")
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := m.createChild(ctx, d.master, args)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- c.ID
		}()
	}
	wg.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("parallel retry created another child")
		}
	}
	second, err := m.createChild(ctx, d.master, childArgs(d, "two"))
	if err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(ctx)
	d.mu.Lock()
	messages := append([]deliveredMessage(nil), d.messages...)
	d.mu.Unlock()
	if len(messages) != 2 || messages[1].UID != d.worker.UID || !strings.Contains(messages[1].Request.Body, first) || !strings.Contains(messages[1].Request.Body, "child_complete") || messages[1].Budget <= 0 {
		t.Fatalf("delivery: %+v", messages)
	}
	if err := m.completeChild(ctx, d.worker, first, "one", "success", "First independent report"); err != nil {
		t.Fatal(err)
	}
	j, err := m.childJoin(ctx, d.master, 1, args.ParentRequestID, true)
	if err != nil {
		t.Fatal(err)
	}
	if j.Evaluation.Open != 1 || d.committed().State == "completed" {
		t.Fatalf("early closure: %+v %+v", j, d.committed())
	}
	d.srv.children.dispatch(ctx)
	if err := m.completeChild(ctx, d.master, second.ID, "two", "success", "forged"); err == nil {
		t.Fatal("parent completed child's task")
	}
	if err := m.completeChild(ctx, d.worker, second.ID, "two", "fail", "Second independent failure"); err != nil {
		t.Fatal(err)
	}
	j, err = m.childJoin(ctx, d.master, 1, args.ParentRequestID, true)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.srv.store.GetSwarmStep(ctx, d.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if j.Evaluation.Open != 0 || v.Outcome == nil || *v.Outcome != "fail" || !strings.Contains(v.Report, first) || !strings.Contains(v.Report, second.ID) {
		t.Fatalf("parent result: %+v %+v", j, v)
	}
}

func TestChildExecutionEngineUnsentAndUncertain(t *testing.T) {
	for _, kind := range []string{"not_sent", "no_receipt", "transport"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			d := newChildEngineServer(t)
			m := d.srv.managed
			d.mu.Lock()
			d.deliver = func(deliveredMessage) (relay.MessageResponse, error) {
				switch kind {
				case "not_sent":
					return relay.MessageResponse{NotSent: true, Verdict: "session unavailable"}, nil
				case "transport":
					return relay.MessageResponse{}, errors.New("private transport failure")
				}
				return relay.MessageResponse{Verdict: "submitted"}, nil
			}
			d.mu.Unlock()
			args := childArgs(d, "one")
			c, err := m.createChild(ctx, d.master, args)
			if err != nil {
				t.Fatal(err)
			}
			d.srv.children.dispatch(ctx)
			p, err := d.srv.store.SwarmChildParentAt(ctx, d.swarm, 1)
			if err != nil {
				t.Fatal(err)
			}
			j, err := d.srv.store.GetSwarmChildJoin(ctx, p, m.childAuthority)
			if err != nil {
				t.Fatal(err)
			}
			want := "unconfirmed"
			if kind == "not_sent" {
				want = "not_sent"
			}
			if j.Children[0].Delivery != want {
				t.Fatalf("delivery: %+v", j)
			}
			_, err = m.cancelChild(ctx, d.master, 1, args.ParentRequestID, "one")
			if kind == "not_sent" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, store.ErrSwarmChildConflict) {
				t.Fatalf("cancelled uncertain effect: %v", err)
			}
			if _, err = m.createChild(ctx, d.master, args); err != nil {
				t.Fatal(err)
			}
			d.srv.children.dispatch(ctx)
			d.mu.Lock()
			n := len(d.messages)
			d.mu.Unlock()
			if n != 2 {
				t.Fatalf("retry resent task: %d", n)
			}
			a, err := d.srv.store.GetManagedActor(ctx, d.worker.UID)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "not_sent" {
				if _, err = d.srv.store.BeginManagedActorOperation(ctx, a.UID, a.Revision, "reset-with-child", "reset"); !errors.Is(err, store.ErrCoordinationConflict) {
					t.Fatalf("reset stranded %s: %v", c.ID, err)
				}
			}
		})
	}
}

func TestChildExecutionEngineChecksSettledExecutorBeforeSend(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	m := d.srv.managed
	args := childArgs(d, "one")
	c, err := m.createChild(ctx, d.master, args)
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.srv.store.SwarmChildParentAt(ctx, d.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	c, err = d.srv.store.StartSwarmChild(ctx, p, c.RequestID, c.Executor, c.Revision, m.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	d.exec(`UPDATE managed_actors SET phase='provisioning',operation='reset' WHERE uid=?`, d.worker.UID)
	d.srv.children.deliver(ctx, store.SwarmChildWork{Parent: p, Task: c})
	d.mu.Lock()
	n := len(d.messages)
	d.mu.Unlock()
	if n != 1 {
		t.Fatal("child sent to an unsettled executor after claim")
	}
}

func TestChildExecutionEngineRevocationBeforeSend(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	m := d.srv.managed
	_, err := m.createChild(ctx, d.master, childArgs(d, "one"))
	if err != nil {
		t.Fatal(err)
	}
	d.exec(`UPDATE users SET disabled=1 WHERE id=?`, d.master.DelegatorID)
	d.srv.children.dispatch(ctx)
	d.mu.Lock()
	n := len(d.messages)
	d.mu.Unlock()
	if n != 1 {
		t.Fatal("revoked scheduling authority sent a child")
	}
}
