package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Гостевой вопрос ephemeral имеет точный child scope, не global fallback.
// SPEC: Исходный child/pass выводится из аутентифицированного UID/inc;
// ответ после completion или без durable journal не уходит через legacy send.
// IMPLEMENTATION: Scoped Store API, child policy внутри FULL prepare и actual transport fence.
// RESULT: IMPLEMENTED — targeted race и полный Go build/test/vet прошли последовательно.
// STATUS: DONE
func ephemeralQuestionServerFixture(t *testing.T) (*dispatchServer, store.EphemeralChildIntent, store.ManagedActor, store.SwarmChildTask, *durableAnswerRelay) {
	t.Helper()
	d, job, child, hub, _ := childDeliveryFixture(t)
	d.srv.steps.close()
	ctx := context.Background()
	if _, err := d.srv.prepareEphemeralChildDelivery(ctx, d.master.DelegatorID, job.Parent, job.ID); err != nil {
		t.Fatal(err)
	}
	actor, err := d.srv.store.GetManagedActor(ctx, child.Executor.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, job, actor, work.Task, hub
}

func TestEphemeralOperatorQuestionUsesRunningChildContext(t *testing.T) {
	d, job, actor, child, _ := ephemeralQuestionServerFixture(t)
	args, err := json.Marshal(map[string]string{"question": "Which isolated target should this child use?", "context": "Original child context."})
	if err != nil {
		t.Fatal(err)
	}
	got := d.srv.managed.handleSwarmRequest(context.Background(), actor.UID, actor.Incarnation, relay.SwarmRequest{Tool: "ask_operator", Arguments: args})
	if !got.OK {
		t.Fatalf("running ephemeral child could not ask a scoped question: %+v", got)
	}
	var response struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(got.Data, &response); err != nil || response.RequestID == "" {
		t.Fatal("question acknowledgment lost request identity")
	}
	q, err := d.srv.store.GetSwarmRequest(context.Background(), d.swarm, response.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := d.srv.store.GetSwarmRequestPassContext(context.Background(), d.swarm, q.ID)
	if err != nil || pass.Pass != job.Parent.Pass || pass.ObservedSequence != job.Parent.Sequence || q.ActorUID != actor.UID || q.ActorIncarnation != actor.Incarnation || q.Sequence != nil || q.Outcome != nil {
		t.Fatalf("guest question lost exact child context or acquired graph authority: %+v %+v %v", q, pass, err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task != child {
		t.Fatal("operator question modified child execution")
	}
}

func TestEphemeralOperatorQuestionWithoutProofIsRefused(t *testing.T) {
	d, _, actor, _, _ := ephemeralQuestionServerFixture(t)
	createSQL(t, d, `DROP TRIGGER ephemeral_child_attestation_update_guard; UPDATE ephemeral_child_attestations SET state='unconfirmed',attested_origin='',attested_commit=''`)
	got := d.srv.managed.handleSwarmRequest(context.Background(), actor.UID, actor.Incarnation, relay.SwarmRequest{Tool: "ask_operator", Arguments: json.RawMessage(`{"question":"Which target?"}`)})
	if got.OK || got.Outcome != "refused" {
		t.Fatalf("missing child source proof was not an explicit refusal: %+v", got)
	}
}

func TestEphemeralOperatorAnswerUsesDurableChildScope(t *testing.T) {
	ctx := context.Background()
	d, _, actor, child, hub := ephemeralQuestionServerFixture(t)
	q, err := d.srv.store.OpenEphemeralChildRequest(ctx, child.ID, store.ActorRequestInput{SwarmID: d.swarm, ActorUID: actor.UID, ActorIncarnation: actor.Incarnation, Kind: "text", Question: "Which isolated target?"}, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	q, err = d.srv.store.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: d.swarm, RequestID: q.ID, UserID: d.master.DelegatorID, Response: "Only the isolated fixture."}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	d.srv.managed.deliverMessage = func(ctx context.Context, uid string, inc int64, frame relay.MessageRequest) (relay.MessageResponse, error) {
		calls.Add(1)
		if uid != actor.UID || inc != actor.Incarnation || frame.DeliveryID == "" || frame.JournalID != hub.journal {
			t.Error("answer escaped its durable recipient/journal")
		}
		scope, ok := ctx.Value(messageSubmissionKey{}).(messageSubmission)
		seen := map[string]bool{}
		for _, key := range scope.actors {
			seen[key] = true
		}
		if !ok || len(seen) != 2 || !seen[actor.UID] || !seen[d.master.UID] || scope.authorize == nil {
			t.Error("answer channel submission omitted parent/executor gates or final authority")
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	user, err := d.srv.store.GetUser(ctx, d.master.DelegatorID)
	if err != nil {
		t.Fatal(err)
	}
	got, reason := d.srv.deliverSwarmAnswer(ctx, user, q, false)
	if calls.Load() != 1 || got.Delivery != "delivered" || reason != "" {
		t.Fatalf("scoped answer delivery: calls=%d delivery=%s reason=%q", calls.Load(), got.Delivery, reason)
	}
}

func TestEphemeralOperatorAnswerNeverUsesLateOrLegacySend(t *testing.T) {
	for _, scenario := range []string{"completed-child", "missing-journal", "revoked-approver"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			d, job, actor, child, hub := ephemeralQuestionServerFixture(t)
			q, err := d.srv.store.OpenEphemeralChildRequest(ctx, child.ID, store.ActorRequestInput{SwarmID: d.swarm, ActorUID: actor.UID, ActorIncarnation: actor.Incarnation, Kind: "text", Question: "Which isolated target?"}, d.srv.managed.childAuthority)
			if err != nil {
				t.Fatal(err)
			}
			q, err = d.srv.store.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: d.swarm, RequestID: q.ID, UserID: d.master.DelegatorID, Response: "Only the isolated fixture."}, func(context.Context) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "completed-child" {
				if err := d.srv.managed.completeChild(ctx, actor, child.ID, child.RequestID, "success", "Independent report before any late answer."); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "missing-journal" {
				hub.mu.Lock()
				hub.journal = ""
				hub.mu.Unlock()
			} else if err := d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			d.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
				calls.Add(1)
				return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
			}
			user, err := d.srv.store.GetUser(ctx, d.master.DelegatorID)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = d.srv.deliverSwarmAnswer(ctx, user, q, false)
			if calls.Load() != 0 {
				t.Fatalf("%s answer bypassed exact child scope/durable transport: %d sends for job %s", scenario, calls.Load(), job.ID)
			}
			if scenario == "revoked-approver" {
				if _, err := d.srv.store.GetActorDeliveryForSource(ctx, d.swarm, "answer", q.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("revoked child authority pinned a fresh answer intent: %v", err)
				}
			}
		})
	}
}
