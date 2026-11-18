package server

import (
	"context"
	"sync"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Конкурирующие запросы не дублируют эфемерный root RPC (#292).
// SPEC: Восемь одинаковых вызовов получают один retained attempt и один эффект.
// IMPLEMENTATION: Свежий durable-резерв допускает только одного отправителя.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralAttestationConcurrentCallsOpenOnlyOneRPC(t *testing.T) {
	d, intent, _ := ephemeralAttestationServerFixture(t)
	before, messages := d.calls()
	type result struct {
		proof store.EphemeralChildAttestation
		err   error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			proof, err := d.srv.attestEphemeralChild(context.Background(), d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1")
			results <- result{proof: proof, err: err}
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got.err != nil || !got.proof.Replay || got.proof.IntentID != intent.ID || got.proof.AttemptID != "rpc-1" || got.proof.State != "pending" && got.proof.State != "attested" {
			t.Fatalf("concurrent call lost retained identity: %+v %v", got.proof, got.err)
		}
	}
	proof, err := d.srv.store.GetEphemeralChildAttestation(context.Background(), intent.ID)
	if err != nil || proof.State != "attested" || proof.AttestedCommit != intent.SourceCommit {
		t.Fatalf("one verified proof missing: %+v %v", proof, err)
	}
	after, sent := d.calls()
	if len(after) != len(before)+1 || len(sent) != len(messages) {
		t.Fatalf("duplicate effect or payload: attests=%d messages=%d", len(after)-len(before), len(sent)-len(messages))
	}
}
