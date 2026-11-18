package server

// FEATURE: Реальный inline review одного artifact в текущем pass (#290).
// SPEC: Подготовка и root result привязаны к H, не к source pass; review не
// заменяет human approval. Известный отказ terminal, unknown не разрешает retry.
// IMPLEMENTATION: Dispatcher выбирает известный root refusal для Store resolution
// и просыпается после committed settlement, не повторяя guest effect.
// RESULT: IMPLEMENTED — full Go/build/vet и focused server/Store race прошли.
// Startup обрабатывает сохранённый refusal без повторного root RPC или инструкции.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/workflowpolicy"
)

const inlineServerGraph = `apiVersion: brood.dev/v1alpha1
name: deliver
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: Build artifact.}
  review: {type: actor, role: review, actionClasses: [review], prompt: Review exact artifact.}
  land: {type: actor, role: build, actionClasses: [landing], prompt: Land reviewed artifact.}
  done: {type: exit}
edges:
  - {from: build, to: review}
  - {from: review, to: land}
  - {from: land, to: done}
goalGates: [done]
`

func inlineServerFixture(t *testing.T, human bool) *resultCollectionFixture {
	t.Helper()
	rules := []workflowpolicy.Rule{{ID: "land-review", Paths: []string{"go/**"}, Require: workflowpolicy.IndependentReview, ReviewRole: "review"}}
	if human {
		rules = append(rules, workflowpolicy.Rule{ID: "storage-human", Paths: []string{"go/**"}, Require: workflowpolicy.Human})
	}
	f := newResultCollectionFixtureWithRules(t, false, inlineServerGraph, rules)
	f.result = func(_ context.Context, _ relay.SessionRequest, result attest.ResultAttestation) (relay.SessionResponse, error) {
		if result.Sequence == 2 {
			result.Head = strings.Repeat("a", 40)
			result.Paths = []string{}
		}
		return inlineSignedResult(t, f, result)
	}
	return f
}

func inlineSignedResult(t *testing.T, f *resultCollectionFixture, result attest.ResultAttestation) (relay.SessionResponse, error) {
	t.Helper()
	signature, err := attest.SignResult(attest.ResultKey(guestToken(f.srv.managed.registry.relaySecret, result.ActorUID, result.Generation)), result.ResultClaim)
	if err != nil {
		t.Fatal(err)
	}
	result.Signature = signature
	body, err := json.Marshal(result)
	return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(body)}, err
}

func inlineComplete(t *testing.T, f *resultCollectionFixture, actor store.ManagedActor, sequence int64, outcome string) {
	t.Helper()
	ctx := context.Background()
	visit, err := f.srv.store.GetSwarmStep(ctx, f.swarm, sequence)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"run_id": f.swarm, "sequence": sequence, "request_id": visit.RequestID, "outcome": outcome, "report": "Actual actor report: " + outcome})
	if err != nil {
		t.Fatal(err)
	}
	response := f.srv.managed.handleSwarmRequest(ctx, actor.UID, actor.Incarnation, relay.SwarmRequest{Tool: "pipeline_visit_complete", Arguments: raw})
	if !response.OK || response.Outcome != "accepted" {
		t.Fatalf("completion %d: %+v", sequence, response)
	}
	receipt, present, err := relay.ReadCompletionReceipt(response.Data)
	if err != nil || !present {
		t.Fatalf("receipt: %v %v", present, err)
	}
	ack, err := relay.CompletionReceiptAck(receipt)
	if err != nil {
		t.Fatal(err)
	}
	response = f.srv.managed.handleSwarmRequest(ctx, actor.UID, actor.Incarnation, ack)
	if !response.OK {
		t.Fatalf("ack %d: %+v", sequence, response)
	}
	f.srv.steps.wait()
}

func inlineAnswer(t *testing.T, f *resultCollectionFixture, sequence int64) {
	t.Helper()
	c, err := f.srv.store.GetPipelinePolicyCandidate(context.Background(), f.swarm, sequence, "result")
	if err != nil {
		t.Fatal(err)
	}
	r, body, _ := f.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/requests/policy:"+c.ID+"/answer", f.csrf(f.alice), map[string]any{"response": "yes", "revision": 1})
	expectStatus(t, "inline policy approval", r, body, http.StatusOK)
	f.srv.steps.wait()
}

func TestInlineServerReviewPinsArtifactAndLands(t *testing.T) {
	for _, human := range []bool{false, true} {
		t.Run(map[bool]string{false: "review", true: "human-and-review"}[human], func(t *testing.T) {
			f := inlineServerFixture(t, human)
			original, err := f.srv.store.GetSwarm(context.Background(), f.swarm)
			if err != nil {
				t.Fatal(err)
			}
			inlineComplete(t, f, f.worker, 1, "success")
			if human {
				if len(f.steps()) != 1 {
					t.Fatal("review bypassed path-human approval")
				}
				inlineAnswer(t, f, 1)
			}
			steps := f.steps()
			attests, messages := f.dispatchServer.calls()
			h := strings.Repeat("a", 40)
			if len(steps) != 2 || steps[1].State != "inflight" || steps[1].ActorUID != f.master.UID || steps[1].Pass != 1 || steps[1].AttestedCommit != h || len(attests) != 2 || attests[1].Args["commit"] != h || attests[1].UID != f.master.UID || len(messages) != 2 {
				t.Fatalf("review was not prepared at exact work H: steps=%+v attests=%+v messages=%d", steps, attests, len(messages))
			}
			candidate, err := f.srv.store.GetPipelinePolicyCandidate(context.Background(), f.swarm, 1, "result")
			if err != nil {
				t.Fatal(err)
			}
			if !human && candidate.Preview.Decision.State != workflowpolicy.WaitingReview {
				t.Fatalf("test did not dispatch a still-obligated review: %+v", candidate.Preview.Decision)
			}
			assignment, err := f.srv.store.GetPipelinePolicyReviewAssignment(context.Background(), f.swarm, candidate.ID, "land-review")
			if err != nil {
				t.Fatal(err)
			}
			if assignment.ReviewSwarmID != f.swarm || assignment.ReviewSequence != 2 || assignment.ActorUID != f.master.UID {
				t.Fatalf("not actual inline review: %+v", assignment)
			}
			inlineComplete(t, f, f.master, 2, "success")
			evidence, err := f.srv.store.GetPipelineResultEvidence(context.Background(), f.swarm, 2)
			if err != nil || evidence.Commit != h || evidence.Head != h || len(evidence.Paths) != 0 {
				t.Fatalf("review root observed wrong base: %+v %v", evidence, err)
			}
			if human {
				if len(f.steps()) != 2 {
					t.Fatal("review bypassed new landing human approval")
				}
				inlineAnswer(t, f, 2)
			}
			steps = f.steps()
			if len(steps) != 3 || steps[2].NodeID != "land" || steps[2].State != "inflight" || steps[1].Outcome == nil || *steps[1].Outcome != "success" || steps[1].Report != "Actual actor report: success" {
				t.Fatalf("review did not release landing: %+v", steps)
			}
			current, err := f.srv.store.GetSwarm(context.Background(), f.swarm)
			if err != nil {
				t.Fatal(err)
			}
			if *current.Source != *original.Source || steps[0].Goal == "" || steps[0].Goal != dispatchGoal {
				t.Fatal("review rewrote immutable source or goal")
			}
			for _, step := range steps {
				if step.Pass != 1 {
					t.Fatal("review created another pass")
				}
			}
			for range 2 {
				f.srv.steps.kick(f.swarm)
				f.srv.steps.wait()
			}
			attests, messages = f.dispatchServer.calls()
			if len(attests) != 3 || len(messages) != 3 || f.calls.Load() != 2 {
				t.Fatalf("extra instruction or observation: %d %d %d", len(attests), len(messages), f.calls.Load())
			}
		})
	}
}

func TestInlineServerReviewUnavailableAfterVerifiedResult(t *testing.T) {
	f := inlineServerFixture(t, false)
	inlineComplete(t, f, f.worker, 1, "success")
	// Новый dispatcher ещё не запущен: ACK не может опередить изменение роли
	// после реально сохранённого root result. Startup обрабатывает observed.
	f.srv.steps.close()
	f.srv.steps = newStepDispatcher(f.srv)
	inlineComplete(t, f, f.master, 2, "success")
	if !f.srv.steps.collectResult(context.Background(), f.swarm) {
		t.Fatal("acknowledged review result was not collected")
	}
	if _, err := f.srv.store.GetPipelineResultEvidence(context.Background(), f.swarm, 2); err != nil {
		t.Fatal(err)
	}
	f.bind("review", f.worker.UID)
	f.srv.steps.start(context.Background())
	f.srv.steps.wait()
	steps := f.steps()
	if f.execution() != "failed" || len(steps) != 2 || steps[1].Outcome == nil || *steps[1].Outcome != "success" || !strings.Contains(steps[1].Detail, "unavailable") {
		t.Fatalf("verified result with unavailable reviewer not terminal: %+v", steps)
	}
	attests, messages := f.dispatchServer.calls()
	if len(attests) != 2 || len(messages) != 2 || f.calls.Load() != 2 {
		t.Fatal("unavailable reviewer was replaced or landing ran")
	}
}

func TestInlineServerReviewPreparationAndDeliveryFailures(t *testing.T) {
	for _, mode := range []string{"prepare-refused", "prepare-unknown", "instruction-refused", "instruction-unknown", "same-author"} {
		t.Run(mode, func(t *testing.T) {
			f := inlineServerFixture(t, false)
			if mode == "same-author" {
				f.bind("review", f.worker.UID)
			}
			f.reply = func(call guestAttest) (relay.SessionResponse, error) {
				if mode == "prepare-refused" {
					return relay.SessionResponse{OK: false, Outcome: "complete", Text: "exact artifact unavailable"}, nil
				}
				if mode == "prepare-unknown" {
					return relay.SessionResponse{Outcome: "unknown"}, nil
				}
				return signedAttestation(t, workspaceAttestationKey(f.srv.managed.registry.relaySecret), call, call.Generation)
			}
			if mode == "instruction-refused" {
				f.onConnected(func(base commandSessionHub, uid string, generation int64) bool {
					return f.committed().State != "delivering" && base.Connected(uid, generation)
				})
			}
			f.deliver = func(deliveredMessage) (relay.MessageResponse, error) {
				if mode == "instruction-unknown" {
					return relay.MessageResponse{}, errors.New("instruction transport unknown")
				}
				return relay.MessageResponse{NotSent: true, Verdict: "instruction refused before submission"}, nil
			}
			inlineComplete(t, f, f.worker, 1, "success")
			for range 2 {
				f.srv.steps.kick(f.swarm)
				f.srv.steps.wait()
			}
			steps := f.steps()
			wantSteps, wantAttests, wantMessages := 2, 2, 1
			if mode == "same-author" {
				wantSteps, wantAttests = 1, 1
			}
			if mode == "instruction-unknown" {
				wantMessages = 2
			}
			attests, messages := f.dispatchServer.calls()
			if len(steps) != wantSteps || len(attests) != wantAttests || len(messages) != wantMessages || f.calls.Load() != 1 {
				t.Fatalf("refused/unknown review repeated effects: steps=%+v effects=%d/%d/%d", steps, len(attests), len(messages), f.calls.Load())
			}
			if strings.HasSuffix(mode, "unknown") {
				if f.execution() != "running" || steps[1].State != "unconfirmed" {
					t.Fatalf("unknown review became terminal: %+v", steps)
				}
			} else if f.execution() != "failed" {
				t.Fatalf("known review unavailability stayed running: %+v", steps)
			}
			if wantSteps == 2 && (steps[1].Outcome != nil || steps[1].Report != "") {
				t.Fatal("transport failure invented actor report/outcome")
			}
		})
	}
}

func TestInlineServerReviewKnownFailuresAndUnknown(t *testing.T) {
	for _, mode := range []string{"negative", "changed-head", "changed-paths", "root-refused", "unknown", "authority-changed"} {
		t.Run(mode, func(t *testing.T) {
			f := inlineServerFixture(t, false)
			inlineComplete(t, f, f.worker, 1, "success")
			if steps := f.steps(); len(steps) != 2 || steps[1].State != "inflight" {
				t.Fatalf("no actual reviewer: %+v", steps)
			}
			f.result = func(_ context.Context, _ relay.SessionRequest, result attest.ResultAttestation) (relay.SessionResponse, error) {
				result.Head = strings.Repeat("a", 40)
				result.Paths = []string{}
				switch mode {
				case "root-refused":
					return relay.SessionResponse{OK: false, Outcome: "complete", Text: "root refused exact observation"}, nil
				case "unknown":
					return relay.SessionResponse{Outcome: "unknown"}, nil
				case "changed-head":
					result.Head = strings.Repeat("b", 40)
				case "changed-paths":
					result.Paths = []string{"go/changed.go"}
				case "authority-changed":
					f.bind("review", f.worker.UID)
				}
				return inlineSignedResult(t, f, result)
			}
			outcome := "success"
			if mode == "negative" {
				outcome = "fail"
			}
			inlineComplete(t, f, f.master, 2, outcome)
			if mode != "unknown" && mode != "authority-changed" && f.execution() != "failed" {
				t.Fatal("known review failure requires an external kick after committed settlement")
			}
			for range 2 {
				f.srv.steps.kick(f.swarm)
				f.srv.steps.wait()
			}
			steps := f.steps()
			execution := f.execution()
			if mode == "unknown" || mode == "authority-changed" {
				// Изменение authority в RPC оставляет effect unknown, не negative.
				if execution != "running" || steps[1].Outcome != nil {
					t.Fatalf("unknown became permission or refusal: %s %+v", execution, steps)
				}
				if _, err := f.srv.store.CurrentActorOperation(context.Background(), f.master.UID); err != nil {
					t.Fatalf("unknown lost lease: %v", err)
				}
			} else {
				if execution != "failed" || len(steps) != 2 || steps[1].Outcome == nil || *steps[1].Outcome != outcome || steps[1].Report != "Actual actor report: "+outcome {
					t.Fatalf("known review failure held or invented outcome: %s %+v", execution, steps)
				}
				if _, err := f.srv.store.CurrentActorOperation(context.Background(), f.master.UID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("known result retained lease: %v", err)
				}
			}
			attests, messages := f.dispatchServer.calls()
			if len(steps) != 2 || len(attests) != 2 || len(messages) != 2 || f.calls.Load() != 2 {
				t.Fatalf("failure repeated/replaced review or landed: steps=%+v effects=%d/%d/%d", steps, len(attests), len(messages), f.calls.Load())
			}
		})
	}
}
