package server

// FEATURE: Guest child_join использует completion receipt и collector (#290).
// SPEC: Все child outcomes сохраняются, parent aggregate computed сервером;
// accepted не означает joined/advanced, root observation только после ACK.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/store"

	"brood.dev/go/brood/proxmox/relay"
)

func TestChildResultCollectorRechecksDelegationBeforeAndAfterRPC(t *testing.T) {
	for _, phase := range []string{"before-rpc", "after-rpc"} {
		t.Run(phase, func(t *testing.T) {
			f := newResultCollectionFixtureWithParent(t, true)
			ctx := context.Background()
			backup := store.User{ID: store.NewID(), Username: "backup-result-admin", PasswordHash: "fixture"}
			if err := f.srv.store.CreateUser(ctx, backup); err != nil {
				t.Fatal(err)
			}
			if err := f.srv.store.SetUserAdmin(ctx, backup.ID, true); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"first", "second"} {
				child, err := f.srv.managed.createChild(ctx, f.master, childArgs(f.dispatchServer, key))
				if err != nil {
					t.Fatal(err)
				}
				f.srv.children.dispatch(ctx)
				if err := f.srv.managed.completeChild(ctx, f.worker, child.ID, key, "success", "Child report"); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(map[string]any{"parent_sequence": 1, "parent_request_id": f.committed().Request, "wait_seconds": 0})
			if err != nil {
				t.Fatal(err)
			}
			response := f.srv.managed.handleSwarmRequest(ctx, f.master.UID, f.master.Incarnation, relay.SwarmRequest{Tool: "child_join", Arguments: raw})
			if response.Outcome != "accepted" {
				t.Fatalf("child completion: %+v", response)
			}
			if phase == "before-rpc" {
				var revoked atomic.Bool
				f.onConnected(func(base commandSessionHub, uid string, generation int64) bool {
					if revoked.CompareAndSwap(false, true) {
						if err := f.srv.store.SetUserAdmin(context.Background(), f.master.DelegatorID, false); err != nil {
							t.Error(err)
						}
					}
					return base.Connected(uid, generation)
				})
			} else {
				f.result = func(ctx context.Context, _ relay.SessionRequest, signed attest.ResultAttestation) (relay.SessionResponse, error) {
					if err := f.srv.store.SetUserAdmin(ctx, f.master.DelegatorID, false); err != nil {
						return relay.SessionResponse{}, err
					}
					raw, err := json.Marshal(signed)
					return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(raw)}, err
				}
			}
			if ack := f.acknowledge(t, response); !ack.OK {
				t.Fatalf("receipt: %+v", ack)
			}
			f.srv.steps.wait()
			observation, err := f.srv.store.GetPipelineResultObservation(ctx, f.swarm, 1)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "before-rpc" {
				if f.calls.Load() != 0 || observation.State != "refused" {
					t.Fatalf("revoked delegation reached result RPC: calls=%d state=%s", f.calls.Load(), observation.State)
				}
			} else if f.calls.Load() != 1 || observation.State != "unconfirmed" {
				t.Fatalf("late private result accepted after delegation revocation: calls=%d state=%s", f.calls.Load(), observation.State)
			}
		})
	}
}

func TestAbortedChildGroupDoesNotRestrictOrdinaryParentResult(t *testing.T) {
	f := newResultCollectionFixtureWithParent(t, true)
	ctx := context.Background()
	if _, err := f.srv.managed.createChild(ctx, f.master, childArgs(f.dispatchServer, "never-started")); err != nil {
		t.Fatal(err)
	}
	r, body, _ := f.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/children/1/abort", f.csrf(f.alice), map[string]any{"revision": 1, "confirm": true})
	expectStatus(t, "owner abort", r, body, http.StatusOK)
	backup := store.User{ID: store.NewID(), Username: "backup-parent-result", PasswordHash: "fixture"}
	if err := f.srv.store.CreateUser(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetUserAdmin(ctx, backup.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetUserAdmin(ctx, f.master.DelegatorID, false); err != nil {
		t.Fatal(err)
	}
	response := f.completion(t)
	if response.Outcome != "accepted" {
		t.Fatalf("ordinary parent result refused after approved abort: %+v", response)
	}
	if ack := f.acknowledge(t, response); !ack.OK {
		t.Fatalf("ordinary parent receipt: %+v", ack)
	}
	f.srv.steps.wait()
	if f.calls.Load() != 1 {
		t.Fatal("aborted child scheduling still restricted the actor's own result")
	}
	if _, err := f.srv.store.GetPipelineResultEvidence(ctx, f.swarm, 1); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyChildJoinCollectsComputedParentResultOnlyAfterAck(t *testing.T) {
	f := newResultCollectionFixtureWithParent(t, true)
	ctx := context.Background()
	m := f.srv.managed
	first, err := m.createChild(ctx, f.master, childArgs(f.dispatchServer, "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.createChild(ctx, f.master, childArgs(f.dispatchServer, "second"))
	if err != nil {
		t.Fatal(err)
	}
	f.srv.children.dispatch(ctx)
	if err := m.completeChild(ctx, f.worker, first.ID, "first", "success", "First report"); err != nil {
		t.Fatal(err)
	}
	f.srv.children.dispatch(ctx)
	if err := m.completeChild(ctx, f.worker, second.ID, "second", "success", "Second report"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"parent_sequence": 1, "parent_request_id": f.committed().Request, "wait_seconds": 0})
	if err != nil {
		t.Fatal(err)
	}
	response := m.handleSwarmRequest(ctx, f.master.UID, f.master.Incarnation, relay.SwarmRequest{Tool: "child_join", Arguments: raw})
	if !response.OK || response.Outcome != "accepted" {
		t.Fatalf("child join must be accepted/pending: %+v", response)
	}
	var status struct {
		Joined bool `json:"joined"`
		Open   int  `json:"open"`
	}
	if json.Unmarshal(response.Data, &status) != nil || status.Joined || status.Open != 0 {
		t.Fatal("child join pretended graph advancement or lost settled obligations")
	}
	if f.calls.Load() != 0 {
		t.Fatal("child join started quiescence before client receipt")
	}
	intent, err := f.srv.store.GetPipelineCompletionIntent(ctx, f.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Outcome != "success" || intent.Report == "" {
		t.Fatal("computed aggregate was not retained")
	}
	ack := f.acknowledge(t, response)
	if !ack.OK {
		t.Fatalf("child completion receipt refused: %+v", ack)
	}
	f.srv.steps.wait()
	if f.calls.Load() != 1 {
		t.Fatalf("child result RPCs=%d", f.calls.Load())
	}
	evidence, err := f.srv.store.GetPipelineResultEvidence(ctx, f.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ActorUID != f.master.UID {
		t.Fatal("child executor artifact substituted for parent result")
	}
	step, err := f.srv.store.GetSwarmStep(ctx, f.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if step.State == "completed" || step.Outcome != nil {
		t.Fatal("child join bypassed policy transition")
	}
}
