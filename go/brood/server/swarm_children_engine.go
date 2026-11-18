package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type childCreateArguments struct {
	Sequence        int64  `json:"parent_sequence"`
	ParentRequestID string `json:"parent_request_id"`
	RequestID       string `json:"request_id"`
	Role            string `json:"role"`
	Task            string `json:"task"`
	Mode            string `json:"mode"`
	Total           int    `json:"total"`
	Quorum          int    `json:"quorum"`
}

func childPrincipal(a store.ManagedActor, project string) store.SwarmChildPrincipal {
	return store.SwarmChildPrincipal{ProjectID: project, SwarmID: a.SwarmID, AtespaceUID: a.AtespaceUID, ActorUID: a.UID, Incarnation: a.Incarnation}
}

// childAuthority is a local, transaction-safe scheduling policy. Masterhood and
// a role binding alone are insufficient: the current approving admin must still
// exist and the parent must retain its exact durable scope. Role routing is the
// server dispatcher's path, not dispatch's guest-owned-worker lease primitive.
func (m *managedProvider) childAuthority(ctx context.Context, a store.SwarmChildAuthorization) error {
	parent, err := m.server.store.GetManagedActor(ctx, a.Parent.Principal.ActorUID)
	if err != nil {
		return store.ErrSwarmChildDenied
	}
	current, sw, err := m.authorizeCommandActorDurable(ctx, parent)
	if err != nil || childPrincipal(current, sw.ProjectID) != a.Parent.Principal || current.Role != "master" || current.DelegatorID == "" {
		return store.ErrSwarmChildDenied
	}
	u, err := m.server.store.GetUser(ctx, current.DelegatorID)
	if err != nil || u.Disabled || !u.IsAdmin {
		return store.ErrSwarmChildDenied
	}
	// Резервы исключают новое автоматическое исполнение, но не чтение
	// истории и запись позднего отчёта прерванной дочерней задачи.
	if a.Action == "reserve" || a.Action == "start" {
		if _, err := m.server.store.CurrentActorOperation(ctx, current.UID); err == nil {
			return store.ErrSwarmChildDenied
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		claims, err := m.server.store.ActorCoordinationClaims(ctx, current.UID)
		if err != nil {
			return err
		}
		if len(claims) != 0 {
			return store.ErrSwarmChildDenied
		}
	}
	if a.Action == "reserve" || a.Action == "start" || a.Action == "complete" {
		target, err := m.server.store.GetManagedActor(ctx, a.Executor.ActorUID)
		if err != nil || target.Role != "worker" || target.UID == current.UID || childPrincipal(target, sw.ProjectID) != a.Executor {
			return store.ErrSwarmChildDenied
		}
		if _, _, err := m.authorizeCommandActorDurable(ctx, target); err != nil {
			return store.ErrSwarmChildDenied
		}
	}
	return nil
}

func (m *managedProvider) withChildParent(ctx context.Context, from store.ManagedActor, sequence int64, request string, fn func(context.Context, store.SwarmChildParent) error) error {
	return m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
		sw, err := m.server.store.GetSwarm(ctx, current.SwarmID)
		if err != nil {
			return err
		}
		v, err := m.server.store.GetSwarmStep(ctx, current.SwarmID, sequence)
		if err != nil {
			return err
		}
		p := store.SwarmChildParent{Principal: childPrincipal(current, sw.ProjectID), Pass: v.Pass, Sequence: sequence, RequestID: request}
		return fn(ctx, p)
	})
}

var errChildNodePolicyMismatch = errors.New("stored child join policy differs from the parent node")

func (m *managedProvider) childNodePolicy(ctx context.Context, p store.SwarmChildParent, total int) (store.SwarmChildJoinPolicy, error) {
	sw, err := m.server.store.GetSwarm(ctx, p.Principal.SwarmID)
	if err != nil {
		return store.SwarmChildJoinPolicy{}, err
	}
	step, err := m.server.store.GetSwarmStep(ctx, sw.ID, p.Sequence)
	if err != nil {
		return store.SwarmChildJoinPolicy{}, err
	}
	graph, err := pipeline.Parse(string(sw.Graph), "pipeline")
	if err != nil {
		return store.SwarmChildJoinPolicy{}, err
	}
	node, ok := graph.Nodes[step.NodeID]
	if !ok || node.Type != "actor" {
		return store.SwarmChildJoinPolicy{}, store.ErrSwarmChildConflict
	}
	// Policy принадлежит снимку узла прохода. Прежние гостевые поля
	// mode/quorum принимаются для совместимости, но не меняют её.
	policy := store.SwarmChildJoinPolicy{Mode: node.ChildJoinMode(), Total: total, Remaining: "retain"}
	if node.ChildJoin != nil {
		policy.Quorum = node.ChildJoin.Quorum
	}
	return policy, nil
}

func (m *managedProvider) createChild(ctx context.Context, from store.ManagedActor, args childCreateArguments) (store.SwarmChildTask, error) {
	if strings.Contains(args.Task, envelopeClosingTag) {
		return store.SwarmChildTask{}, store.ErrSwarmChildConflict
	}
	var c store.SwarmChildTask
	err := m.withChildParent(ctx, from, args.Sequence, args.ParentRequestID, func(ctx context.Context, p store.SwarmChildParent) error {
		policy, err := m.childNodePolicy(ctx, p, args.Total)
		if err != nil {
			return err
		}
		c, err = m.server.store.EnqueueSwarmChild(ctx, p, policy, args.RequestID, args.Role, args.Task, m.childAuthority)
		return err
	})
	if err != nil {
		return store.SwarmChildTask{}, err
	}
	return c, nil
}

func (m *managedProvider) childJoin(ctx context.Context, from store.ManagedActor, sequence int64, request string, advance bool) (store.SwarmChildJoin, error) {
	var j store.SwarmChildJoin
	err := m.withChildParent(ctx, from, sequence, request, func(ctx context.Context, p store.SwarmChildParent) error {
		var err error
		j, err = m.server.store.GetSwarmChildJoin(ctx, p, m.childAuthority)
		if err != nil || !advance {
			return err
		}
		policy, err := m.childNodePolicy(ctx, p, j.Policy.Total)
		if err != nil {
			return err
		}
		// Старую несовместимую policy не переписываем: история и отчёты
		// остаются доступными, но автоматический переход требует сверки.
		if policy != j.Policy {
			return errChildNodePolicyMismatch
		}
		j, err = m.server.store.JoinSwarmChildren(ctx, p, m.childAuthority)
		return err
	})
	if err == nil && advance && j.Evaluation.Open == 0 && !j.Interrupted && j.State != "aborted" {
		m.server.steps.kick(from.SwarmID)
	}
	return j, err
}

func (m *managedProvider) cancelChild(ctx context.Context, from store.ManagedActor, sequence int64, request, key string) (store.SwarmChildTask, error) {
	var result store.SwarmChildTask
	err := m.withChildParent(ctx, from, sequence, request, func(ctx context.Context, p store.SwarmChildParent) error {
		j, err := m.server.store.GetSwarmChildJoin(ctx, p, m.childAuthority)
		if err != nil {
			return err
		}
		for _, c := range j.Children {
			if c.RequestID == key {
				expected := c.Revision
				if c.State == "cancelled" {
					expected--
				}
				result, err = m.server.store.CancelSwarmChild(ctx, p, key, expected, m.childAuthority)
				return err
			}
		}
		return store.ErrNotFound
	})
	return result, err
}

func (m *managedProvider) completeChild(ctx context.Context, from store.ManagedActor, id, key, outcome, report string) error {
	err := m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
		w, err := m.server.store.SwarmChildWorkByID(ctx, id)
		if err != nil {
			return err
		}
		if w.Task.RequestID != key || w.Task.Executor != childPrincipal(current, w.Parent.Principal.ProjectID) {
			return store.ErrSwarmChildDenied
		}
		expected := w.Task.Revision
		if w.Task.State == "completed" {
			expected--
		}
		_, err = m.server.store.CompleteSwarmChild(ctx, w.Parent, key, w.Task.Executor, expected, outcome, report, m.childAuthority)
		return err
	})
	if err == nil {
		m.server.children.kick()
	}
	return err
}

// childDispatcher bounds external calls globally to eight and uses durable FIFO
// executor heads. No worker is provisioned or workspace reset by child dispatch.
// A receipt is delivery evidence, not task completion or workspace attestation.
type childDispatcher struct {
	server *Server
	slots  chan struct{}
	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	scanMu sync.Mutex
	cursor int64
}

func newChildDispatcher(s *Server) *childDispatcher {
	return &childDispatcher{server: s, slots: make(chan struct{}, 8), wake: make(chan struct{}, 1)}
}
func (d *childDispatcher) start(ctx context.Context) {
	ctx, d.cancel = context.WithCancel(ctx)
	d.done = make(chan struct{})
	go func() {
		defer close(d.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			d.dispatch(ctx)
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
			case <-ticker.C:
			}
		}
	}()
}
func (d *childDispatcher) kick() {
	if d == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}
func (d *childDispatcher) close() {
	if d != nil && d.cancel != nil {
		d.cancel()
		<-d.done
	}
}
func (d *childDispatcher) dispatch(ctx context.Context) {
	d.scanMu.Lock()
	work, err := d.server.store.QueuedSwarmChildrenAfter(ctx, d.cursor)
	if err == nil {
		if len(work) == 0 {
			d.cursor = 0
		} else {
			d.cursor = work[len(work)-1].QueuePosition
		}
	}
	d.scanMu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("child queue read failed", "err", err.Error())
		}
		return
	}
	var wg sync.WaitGroup
	for _, w := range work {
		if ctx.Err() != nil {
			break
		}
		select {
		case d.slots <- struct{}{}:
		default:
			continue
		}
		c, err := d.server.store.StartSwarmChild(ctx, w.Parent, w.Task.RequestID, w.Task.Executor, w.Task.Revision, d.server.managed.childAuthority)
		if err != nil {
			<-d.slots
			continue
		}
		w.Task = c
		wg.Add(1)
		go func(w store.SwarmChildWork) { defer wg.Done(); defer func() { <-d.slots }(); d.deliver(ctx, w) }(w)
	}
	wg.Wait()
}

func (d *childDispatcher) check(ctx context.Context, w store.SwarmChildWork) error {
	parent, err := d.server.store.GetManagedActor(ctx, w.Parent.Principal.ActorUID)
	if err != nil {
		return store.ErrSwarmChildDenied
	}
	if _, _, err = d.server.managed.authorizeCommandActor(ctx, parent); err != nil {
		return err
	}
	return d.server.store.WithTx(ctx, func(ctx context.Context) error {
		j, err := d.server.store.GetSwarmChildJoin(ctx, w.Parent, d.server.managed.childAuthority)
		if err != nil {
			return err
		}
		if j.Interrupted {
			return store.ErrSwarmChildConflict
		}
		binding, err := d.server.store.ResolveSwarmRole(ctx, w.Parent.Principal.SwarmID, w.Task.Role)
		if err != nil {
			return err
		}
		if binding.ActorUID != w.Task.Executor.ActorUID || binding.ActorIncarnation != w.Task.Executor.Incarnation {
			return store.ErrSwarmChildDenied
		}
		// Repeat scheduling authority with the exact target inside the write snapshot.
		if err := d.server.managed.childAuthority(ctx, store.SwarmChildAuthorization{Action: "start", Parent: w.Parent, Executor: w.Task.Executor}); err != nil {
			return err
		}
		for _, c := range j.Children {
			if c.ID == w.Task.ID && c.State == "running" && c.Delivery == "sending" {
				return nil
			}
		}
		return store.ErrSwarmChildConflict
	})
}

func childInstruction(w store.SwarmChildWork) string {
	return fmt.Sprintf("[brood child task %s] Parent step %d, pass %d.\n\n%s\n\nThis is an independent child task, not a graph step. Report once with child_complete: child_id %s, request_id %s, outcome success or fail, and report describing the result. Do not complete the parent's pipeline visit. Delivery is not completion. The allocation is reserved, but workspace cleanliness and source checkout are not attested by this message.", w.Task.ID, w.Parent.Sequence, w.Parent.Pass, w.Task.Payload, w.Task.ID, w.Task.RequestID)
}
func (d *childDispatcher) deliver(ctx context.Context, w store.SwarmChildWork) {
	s := d.server
	delivery, detail := "not_sent", "The child instruction was not sent; cancel this unsent intent explicitly."
	if ctx.Err() == nil {
		resolve := func(ctx context.Context) (store.ManagedActor, error) {
			a, err := s.store.GetManagedActor(ctx, w.Task.Executor.ActorUID)
			if err != nil || a.Incarnation != w.Task.Executor.Incarnation {
				return store.ManagedActor{}, app.ErrNotFound
			}
			return a, nil
		}
		dctx, cancel := context.WithTimeout(ctx, requestDeliveryTimeout)
		dctx = context.WithValue(dctx, actorMessageScopeKey{}, []string{w.Parent.Principal.ActorUID})
		result, sent, err := s.deliverToActor(dctx, w.Parent.Principal.SwarmID, resolve, relay.MessageRequest{From: stepMessageSender(w.Parent.Principal.SwarmID), Body: childInstruction(w)}, func(ctx context.Context) error { return d.check(ctx, w) })
		cancel()
		if sent {
			delivery = "unconfirmed"
			detail = "Child delivery is unconfirmed; the task may have reached the actor. It is not resent and its allocation remains held until completion or reconciliation."
			if err == nil && result.Outcome == "completed" {
				delivery = "delivered"
				detail = "The actor confirmed receipt; execution and its independent report are still outstanding."
			}
		}
	}
	bctx, cancel := bookkeeping(ctx)
	defer cancel()
	if err := s.store.RecordSwarmChildDelivery(bctx, w.Parent, w.Task.RequestID, delivery, detail); err != nil && !errors.Is(err, store.ErrSwarmChildConflict) {
		slog.Error("child delivery outcome was not recorded", "child", w.Task.ID, "err", err.Error())
	}
}
