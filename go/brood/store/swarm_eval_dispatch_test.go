package store

import (
	"context"
	"testing"
)

// FEATURE: Bounded restart discovery of eval questions.
// SPEC: Only active evals with open requests are eligible; stopped decisions stay stopped.
// IMPLEMENTATION: Read-only query capped at 256 candidates, without schema changes.
// RESULT: IMPLEMENTED — focused normal/race and full Go suite pass.
// STATUS: DONE
func TestWaitingSwarmEvalDiscovery(t *testing.T) {
	s, in := evalWaiting(t, startApprove)
	ids, err := s.WaitingSwarmEvalSwarms(context.Background(), "")
	if err != nil || len(ids) != 1 || ids[0] != in.CandidateSwarmID {
		t.Fatalf("waiting: %v %v", ids, err)
	}
	if _, err = s.ApplySwarmEvalReplay(context.Background(), in, evalAuthorize, evalAuthorize); err != nil {
		t.Fatal(err)
	}
	ids, err = s.WaitingSwarmEvalSwarms(context.Background(), "")
	if err != nil || len(ids) != 0 {
		t.Fatalf("settled: %v %v", ids, err)
	}
}
