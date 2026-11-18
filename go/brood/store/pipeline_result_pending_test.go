package store

// FEATURE: Collector выбирает только acknowledged и ещё не наблюдавшиеся intent (#290).
// SPEC: До ACK очередь пуста; после любого observation повтора нет; stopped и
// interrupted проходы не выбираются, время не заменяет receipt.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
)

func TestPendingPipelineResultRequiresAcknowledgementAndNoPreviousObservation(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	receipt, err := s.AcceptPipelineCompletion(ctx, in, completionNonce(), allow)
	mustNil(t, err)
	if _, err := s.PendingPipelineResultIntent(ctx, "one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unacknowledged intent entered queue: %v", err)
	}
	swarms, err := s.PendingPipelineResultSwarms(ctx)
	mustNil(t, err)
	if len(swarms) != 0 {
		t.Fatal("unacknowledged swarm queued")
	}
	_, err = s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	pending, err := s.PendingPipelineResultIntent(ctx, "one")
	mustNil(t, err)
	if pending != receipt.Intent {
		t.Fatal("queue changed exact completion identity")
	}
	swarms, err = s.PendingPipelineResultSwarms(ctx)
	mustNil(t, err)
	if !reflect.DeepEqual(swarms, []string{"one"}) {
		t.Fatalf("queue: %v", swarms)
	}
	intent := receipt.Intent
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: in.ActorUID, Generation: in.Incarnation, Swarm: in.SwarmID, Step: in.RequestID, Origin: intent.Origin, Commit: intent.BaseCommit, Challenge: completionNonce()}, Pass: intent.Pass, Sequence: in.Sequence}
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-once", "caller", request)
	mustNil(t, err)
	mustNil(t, s.SettlePipelineResultObservation(ctx, observation, false, "unknown effect"))
	if _, err := s.PendingPipelineResultIntent(ctx, "one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncertain observation queued again: %v", err)
	}
	swarms, err = s.PendingPipelineResultSwarms(ctx)
	mustNil(t, err)
	if len(swarms) != 0 {
		t.Fatal("uncertain observation scheduled again")
	}
}

func TestPendingPipelineResultExcludesStoppedPass(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	receipt, err := s.AcceptPipelineCompletion(ctx, in, completionNonce(), allow)
	mustNil(t, err)
	_, err = s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	stopOne(t, s)
	if _, err := s.PendingPipelineResultIntent(ctx, "one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stopped intent queued: %v", err)
	}
}
