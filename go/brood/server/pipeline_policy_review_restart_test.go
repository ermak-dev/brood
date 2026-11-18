package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
)

// BUG: Committed root refusal не завершает inline review после утраты wakeup.
// SPEC: Startup возобновляет только bookkeeping, без новой инструкции/root RPC.
func TestInlineServerKnownRefusalSurvivesLostKick(t *testing.T) {
	ctx := context.Background()
	f := inlineServerFixture(t, false)
	inlineComplete(t, f, f.worker, 1, "success")
	f.srv.steps.close()
	f.srv.steps = newStepDispatcher(f.srv)
	f.result = func(context.Context, relay.SessionRequest, attest.ResultAttestation) (relay.SessionResponse, error) {
		return relay.SessionResponse{OK: false, Outcome: "complete", Text: "known root refusal before restart"}, nil
	}
	inlineComplete(t, f, f.master, 2, "success")
	if !f.srv.steps.collectResult(ctx, f.swarm) {
		t.Fatal("no review result collection")
	}
	observation, err := f.srv.store.GetPipelineResultObservation(ctx, f.swarm, 2)
	if err != nil || observation.State != "refused" || f.execution() != "running" {
		t.Fatalf("fixture has no committed refusal awaiting bookkeeping: %+v %v", observation, err)
	}
	attests, messages := f.dispatchServer.calls()
	calls := f.calls.Load()
	for range 2 {
		f.srv.steps.start(ctx)
		f.srv.steps.wait()
		steps := f.steps()
		if f.execution() != "failed" || len(steps) != 2 || steps[1].State != "completed" || steps[1].Outcome == nil || *steps[1].Outcome != "success" || steps[1].Report != "Actual actor report: success" {
			t.Fatalf("startup omitted refusal or rewrote actual actor outcome: execution=%s steps=%+v", f.execution(), steps)
		}
		afterAttests, afterMessages := f.dispatchServer.calls()
		if f.calls.Load() != calls || len(afterAttests) != len(attests) || len(afterMessages) != len(messages) {
			t.Fatal("startup repeated result collection, preparation or instruction")
		}
	}
}
