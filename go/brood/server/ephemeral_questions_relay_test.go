package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Scoped вопросы и ответы проходят реальные TLS/WSS/SSH и journal (#292).
// SPEC: Потерянная квитанция даёт только probe; завершённое вмешательство parent
// или executor до настоящего channel-open исключает позднюю запись в модель.
// IMPLEMENTATION: Настоящий relay и файловый journal, только model sink симулируется.
// RESULT: IMPLEMENTED — одна запись при потерянной квитанции, ноль после owner-вмешательства.
// STATUS: DONE
func ephemeralQuestionRealRelayFixture(t *testing.T) (*dispatchServer, store.ManagedActor, store.SwarmChildTask, store.SwarmRequest, *childDeliveryReceiver, *atomic.Int64) {
	t.Helper()
	d, job, child := ephemeralChildServerFixture(t)
	d.srv.steps.close()
	d.srv.actorDelivery.close()
	receiver := scopedDeliveryRealRelay(t, d, child, "answer")
	clock := new(atomic.Int64)
	clock.Store(time.Now().UTC().UnixNano())
	d.srv.actorDelivery = newActorDeliveryService(d.srv, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	ctx := context.Background()
	if _, err := d.srv.prepareEphemeralChildDelivery(ctx, d.master.DelegatorID, job.Parent, job.ID); err != nil {
		t.Fatal(err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := childCall(t, receiver.executor, "ask_operator", map[string]any{"question": "Which isolated target should this exact child use?", "context": "No graph routing or landing authority."})
	var opened struct {
		ID string `json:"request_id"`
	}
	if !got.OK || json.Unmarshal(got.Data, &opened) != nil || opened.ID == "" {
		t.Fatalf("real guest could not open child-scoped request: %+v", got)
	}
	q, err := d.srv.store.GetSwarmRequest(ctx, d.swarm, opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, d.worker, work.Task, q, receiver, clock
}

func TestEphemeralOperatorAnswerRealLostReceiptOnlyProbes(t *testing.T) {
	d, _, child, q, receiver, clock := ephemeralQuestionRealRelayFixture(t)
	actual := d.srv.managed.deliverMessage
	var lost atomic.Bool
	lost.Store(true)
	d.srv.managed.deliverMessage = func(ctx context.Context, uid string, inc int64, frame relay.MessageRequest) (relay.MessageResponse, error) {
		response, err := actual(ctx, uid, inc, frame)
		if err == nil && lost.CompareAndSwap(true, false) {
			return relay.MessageResponse{}, io.ErrUnexpectedEOF
		}
		return response, err
	}
	path := "/api/swarms/" + d.swarm + "/requests/" + q.ID + "/answer"
	response, body, _ := d.do(d.alice, "POST", path, d.csrf(d.alice), map[string]any{"response": "Use only the isolated fixture target."})
	expectStatus(t, "real scoped human answer", response, body, http.StatusOK)
	ctx := context.Background()
	q, err := d.srv.store.GetSwarmRequest(ctx, d.swarm, q.ID)
	if err != nil || q.State != "answered" || q.Delivery != "unconfirmed" || receiver.writes.Load() != 1 {
		t.Fatalf("lost receipt fabricated completion or lost human answer: %+v %v writes=%d", q, err, receiver.writes.Load())
	}
	clock.Add(int64(time.Hour))
	user, err := d.srv.store.GetUser(ctx, d.master.DelegatorID)
	if err != nil {
		t.Fatal(err)
	}
	got, reason := d.srv.deliverSwarmAnswer(ctx, user, q, true)
	if got.Delivery != "delivered" || reason != "" || receiver.writes.Load() != 1 {
		t.Fatalf("unknown answer was resent instead of probed: %+v %q writes=%d", got, reason, receiver.writes.Load())
	}
	receiver.mu.Lock()
	messages := append([]relay.MessageRequest(nil), receiver.messages...)
	receiver.mu.Unlock()
	if len(messages) != 2 || messages[0].Replay || !messages[1].Replay || messages[0].DeliveryID != messages[1].DeliveryID || messages[0].Body != messages[1].Body {
		t.Fatalf("original answer/probe identity changed: %+v", messages)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || work.Task != child {
		t.Fatal("answer receipt completed or modified independent child work")
	}
}

func TestEphemeralOperatorAnswerRealOpenFencesParentAndExecutor(t *testing.T) {
	for _, target := range []string{"parent", "executor"} {
		t.Run(target, func(t *testing.T) {
			d, actor, _, q, receiver, _ := ephemeralQuestionRealRelayFixture(t)
			ctx := context.Background()
			q, err := d.srv.store.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: d.swarm, RequestID: q.ID, UserID: d.master.DelegatorID, Response: "Only the isolated target."}, func(context.Context) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			actual := d.srv.managed.deliverMessage
			entered, resume := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
			}()
			d.srv.managed.deliverMessage = func(ctx context.Context, uid string, inc int64, frame relay.MessageRequest) (relay.MessageResponse, error) {
				close(entered)
				select {
				case <-resume:
				case <-ctx.Done():
					return relay.MessageResponse{}, ctx.Err()
				}
				return actual(ctx, uid, inc, frame)
			}
			user, err := d.srv.store.GetUser(ctx, d.master.DelegatorID)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); _, _ = d.srv.deliverSwarmAnswer(ctx, user, q, false) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("answer did not reach delayed adapter")
			}
			if target == "parent" {
				actor = d.master
			}
			ownerCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err = d.srv.executeOperatorSessionCommand(ownerCtx, d.master.DelegatorID, d.swarm, "operator:scoped-answer-fence", coordinationInvocation("slash", map[string]any{"name": actor.Name, "command": "/status"}))
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			close(resume)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("answer did not finish after owner intervention")
			}
			receiver.mu.Lock()
			opened := len(receiver.messages)
			receiver.mu.Unlock()
			if receiver.writes.Load() != 0 || opened != 0 {
				t.Fatalf("late answer passed completed %s intervention: writes=%d channels=%d", target, receiver.writes.Load(), opened)
			}
		})
	}
}
