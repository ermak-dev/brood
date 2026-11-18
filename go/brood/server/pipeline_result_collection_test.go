package server

// FEATURE: Completion acceptance, client ACK и root result collection (#290).
// SPEC: До ACK нет quiescence; после него один signed result RPC вне транзакции,
// exact binding и сохранённое evidence. Это ещё не разрешение перехода графа.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/workflowpolicy"
)

type resultCollectionFixture struct {
	*dispatchServer
	calls  atomic.Int32
	parent store.ManagedActor
	result func(context.Context, relay.SessionRequest, attest.ResultAttestation) (relay.SessionResponse, error)
}

func newResultCollectionFixture(t *testing.T) *resultCollectionFixture {
	t.Helper()
	return newResultCollectionFixtureWithParent(t, false)
}

func newResultCollectionFixtureWithParent(t *testing.T, masterParent bool) *resultCollectionFixture {
	t.Helper()
	return newResultCollectionFixtureWithGraph(t, masterParent, dispatchGraph)
}

func newResultCollectionFixtureWithGraph(t *testing.T, masterParent bool, sourceGraph string) *resultCollectionFixture {
	t.Helper()
	return newResultCollectionFixtureWithRules(t, masterParent, sourceGraph, []workflowpolicy.Rule{{ID: "storage", Paths: []string{"go/**"}, Require: workflowpolicy.Human}})
}

func newResultCollectionFixtureWithRules(t *testing.T, masterParent bool, sourceGraph string, rules []workflowpolicy.Rule) *resultCollectionFixture {
	t.Helper()
	d := newDispatchServer(t)
	parent := d.worker
	if masterParent {
		d.srv.children.close()
		d.srv.children = newChildDispatcher(d.srv)
		d.bind("build", d.master.UID)
		d.bind("review", d.worker.UID)
		parent = d.master
	}
	f := &resultCollectionFixture{dispatchServer: d, parent: parent}
	ctx := context.Background()
	swarm, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.SetProjectWorkflowPolicy(ctx, root.ID, swarm.ProjectID, 0, rules, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(sourceGraph, "role: build,", "role: build, actionClasses: [work],", 1)
	if _, err := d.srv.store.SyncWorkflowDefinitions(ctx, swarm.ProjectID, root.ID, map[string]string{"deliver": graph}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	original := d.srv.managed.hub
	d.srv.managed.hub = commandSessionHub{guestRelay: original, call: func(ctx context.Context, uid string, generation int64, request relay.SessionRequest) (relay.SessionResponse, error) {
		if request.Operation != relay.WorkspaceResultOperation {
			return d.callSession(ctx, uid, generation, request)
		}
		f.calls.Add(1)
		var args struct {
			Swarm     string `json:"swarm"`
			Step      string `json:"step"`
			Origin    string `json:"origin"`
			Commit    string `json:"commit"`
			Challenge string `json:"challenge"`
			Pass      int64  `json:"pass"`
			Sequence  int64  `json:"sequence"`
		}
		if json.Unmarshal(request.Arguments, &args) != nil {
			return relay.SessionResponse{}, errors.New("invalid result request")
		}
		db, err := sql.Open("sqlite", d.dbPath)
		if err != nil {
			return relay.SessionResponse{}, err
		}
		defer db.Close()
		check, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var acknowledged, state string
		err = db.QueryRowContext(check, `SELECT a.acknowledged_at,o.state FROM pipeline_completion_acknowledgements a JOIN pipeline_result_observations o ON o.swarm_id=a.swarm_id AND o.sequence=a.sequence WHERE a.swarm_id=? AND a.sequence=?`, args.Swarm, args.Sequence).Scan(&acknowledged, &state)
		if err != nil || acknowledged == "" || state != "inflight" {
			t.Error("result RPC ran before receipt/reservation commit")
			return relay.SessionResponse{}, errors.New("missing committed receipt")
		}
		claim := attest.ResultClaim{ResultRequest: attest.ResultRequest{Claim: attest.Claim{ActorUID: uid, Generation: generation, Swarm: args.Swarm, Step: args.Step, Origin: args.Origin, Commit: args.Commit, Challenge: args.Challenge}, Pass: args.Pass, Sequence: args.Sequence}, Head: strings.Repeat("a", 40), Paths: []string{"go/result.go"}}
		signature, err := attest.SignResult(attest.ResultKey(guestToken(d.srv.managed.registry.relaySecret, uid, generation)), claim)
		if err != nil {
			return relay.SessionResponse{}, err
		}
		signed := attest.ResultAttestation{ResultClaim: claim, Signature: signature}
		if f.result != nil {
			return f.result(ctx, request, signed)
		}
		data, err := json.Marshal(signed)
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(data)}, err
	}}
	d.start("deliver")
	return f
}

func (f *resultCollectionFixture) completion(t *testing.T) relay.SwarmResponse {
	t.Helper()
	steps, err := f.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"run_id": f.swarm, "sequence": steps[0].Sequence, "request_id": steps[0].RequestID, "outcome": "success", "report": "Candidate is committed; collect exact result evidence."})
	if err != nil {
		t.Fatal(err)
	}
	return f.srv.managed.handleSwarmRequest(context.Background(), f.parent.UID, f.parent.Incarnation, relay.SwarmRequest{Tool: "pipeline_visit_complete", Arguments: raw})
}
func (f *resultCollectionFixture) acknowledge(t *testing.T, response relay.SwarmResponse) relay.SwarmResponse {
	t.Helper()
	receipt, present, err := relay.ReadCompletionReceipt(response.Data)
	if err != nil || !present {
		t.Fatalf("accepted completion has no receipt: %+v %v", response, err)
	}
	request, err := relay.CompletionReceiptAck(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return f.srv.managed.handleSwarmRequest(context.Background(), f.parent.UID, f.parent.Incarnation, request)
}

func TestPipelineCompletionCollectsResultOnlyAfterDurableClientAck(t *testing.T) {
	f := newResultCollectionFixture(t)
	response := f.completion(t)
	if !response.OK || response.Outcome != "accepted" {
		t.Fatalf("completion must be accepted/pending, not completed: %+v", response)
	}
	f.srv.steps.kick(f.swarm)
	f.srv.steps.wait()
	if f.calls.Load() != 0 {
		t.Fatal("completion receipt was inferred before client acknowledgement")
	}
	retry := f.completion(t)
	if string(retry.Data) != string(response.Data) {
		t.Fatal("exact completion retry changed its receipt")
	}
	ack := f.acknowledge(t, response)
	if !ack.OK || ack.Outcome != "confirmed" {
		t.Fatalf("receipt rejected: %+v", ack)
	}
	f.srv.steps.wait()
	if f.calls.Load() != 1 {
		t.Fatalf("result calls=%d, want one", f.calls.Load())
	}
	evidence, err := f.srv.store.GetPipelineResultEvidence(context.Background(), f.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ActorUID != f.worker.UID || evidence.Generation != f.worker.Incarnation || evidence.Pass != 1 || evidence.Sequence != 1 || evidence.Commit != dispatchCommit || evidence.Head != strings.Repeat("a", 40) {
		t.Fatal("collected another artifact or attempt")
	}
	steps, err := f.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].State != "inflight" || steps[0].Outcome != nil {
		t.Fatal("result collection bypassed policy transition")
	}
	if _, err := f.srv.store.CurrentActorOperation(context.Background(), f.worker.UID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("successful result did not release its lease: %v", err)
	}
	ack = f.acknowledge(t, response)
	if !ack.OK {
		t.Fatal("exact ACK retry was not idempotent")
	}
	f.srv.steps.wait()
	if f.calls.Load() != 1 {
		t.Fatal("duplicate ACK repeated result quiescence")
	}
}

func TestPipelineResultCollectorResumesAcknowledgedUnreservedWork(t *testing.T) {
	f := newResultCollectionFixture(t)
	response := f.completion(t)
	metadata, present, err := relay.ReadCompletionReceipt(response.Data)
	if err != nil || !present {
		t.Fatal("completion did not retain acceptance metadata")
	}
	_, err = f.srv.store.AcknowledgePipelineCompletion(context.Background(), store.PipelineCompletionAcknowledgement{Version: metadata.Version, SwarmID: metadata.SwarmID, Pass: metadata.Pass, Sequence: metadata.Sequence, RequestID: metadata.RequestID, ActorUID: f.parent.UID, Incarnation: f.parent.Incarnation, Nonce: metadata.Nonce}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("store acknowledgement invoked an external effect")
	}
	if err := f.srv.steps.kickPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.srv.steps.wait()
	if f.calls.Load() != 1 {
		t.Fatal("startup pending scan lost acknowledged unreserved work")
	}
}

func TestPipelineCompletionReceiptRefusesOtherActorAndLateFirstAck(t *testing.T) {
	for _, kind := range []string{"other-actor", "owner-stop"} {
		t.Run(kind, func(t *testing.T) {
			f := newResultCollectionFixture(t)
			response := f.completion(t)
			metadata, present, err := relay.ReadCompletionReceipt(response.Data)
			if err != nil || !present {
				t.Fatal("missing acceptance receipt")
			}
			request, err := relay.CompletionReceiptAck(metadata)
			if err != nil {
				t.Fatal(err)
			}
			actor := f.parent
			if kind == "other-actor" {
				actor = f.master
			} else {
				f.stop()
			}
			ack := f.srv.managed.handleSwarmRequest(context.Background(), actor.UID, actor.Incarnation, request)
			if ack.OK {
				t.Fatal("foreign or late first receipt accepted")
			}
			f.srv.steps.wait()
			if f.calls.Load() != 0 {
				t.Fatal("unauthorized acknowledgement reached root observation")
			}
			receipt, err := f.srv.store.GetPipelineCompletionReceipt(context.Background(), f.swarm, metadata.Sequence)
			if err != nil || receipt.AcknowledgedAt != "" {
				t.Fatal("rejected acknowledgement changed durable receipt")
			}
		})
	}
}

func TestPipelineResultCollectorRetainsUncertaintyAndDoesNotRetry(t *testing.T) {
	for _, kind := range []string{"transport", "signature", "dirty", "owner-stop"} {
		t.Run(kind, func(t *testing.T) {
			f := newResultCollectionFixture(t)
			f.result = func(ctx context.Context, _ relay.SessionRequest, signed attest.ResultAttestation) (relay.SessionResponse, error) {
				switch kind {
				case "transport":
					return relay.SessionResponse{}, errors.New("synthetic lost relay result")
				case "signature":
					signed.Signature = "invalid"
				case "dirty":
					return relay.SessionResponse{Outcome: "failed", Text: "workspace is dirty"}, nil
				case "owner-stop":
					swarm, err := f.srv.store.GetSwarm(ctx, f.swarm)
					if err != nil {
						return relay.SessionResponse{}, err
					}
					if _, err := f.srv.store.StopSwarm(ctx, f.swarm, swarm.OwnerID, func(context.Context) bool { return true }); err != nil {
						return relay.SessionResponse{}, err
					}
				}
				raw, err := json.Marshal(signed)
				return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(raw)}, err
			}
			response := f.completion(t)
			if response.Outcome != "accepted" {
				t.Fatalf("completion: %+v", response)
			}
			ack := f.acknowledge(t, response)
			if !ack.OK {
				t.Fatalf("acknowledgement: %+v", ack)
			}
			f.srv.steps.wait()
			observation, err := f.srv.store.GetPipelineResultObservation(context.Background(), f.swarm, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := "unconfirmed"
			if kind == "dirty" {
				want = "refused"
			}
			if observation.State != want {
				t.Fatalf("state=%s want=%s", observation.State, want)
			}
			_, err = f.srv.store.CurrentActorOperation(context.Background(), f.worker.UID)
			if kind == "dirty" {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatal("known refusal retained lease")
				}
			} else if err != nil {
				t.Fatal("unknown effect released lease")
			}
			f.srv.steps.kick(f.swarm)
			f.srv.steps.wait()
			if f.calls.Load() != 1 {
				t.Fatal("refused or unknown observation was retried")
			}
		})
	}
}
