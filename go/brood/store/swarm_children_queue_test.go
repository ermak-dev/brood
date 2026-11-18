package store

// FEATURE: Bounded FIFO queue windows support fair dispatcher rotation (#289).
// SPEC: The private scan cursor never skips an older child of the same worker
// or turns a later queued task into authority to execute it.
// IMPLEMENTATION: Bounded keyset windows preserve the per-allocation FIFO head.
// RESULT: IMPLEMENTED — full Go suite and focused race checks pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"testing"
)

func TestChildEngineQueueCursorPreservesExecutorFIFO(t *testing.T) {
	ctx := context.Background()
	s, p, _ := engineFixture(t)
	a := engineChild(t, s, p, "a")
	b := engineChild(t, s, p, "b")
	head, err := s.QueuedSwarmChildren(ctx)
	mustNil(t, err)
	if len(head) != 1 || head[0].Task.ID != a.ID || head[0].QueuePosition <= 0 {
		t.Fatalf("FIFO head: %+v", head)
	}
	tail, err := s.QueuedSwarmChildrenAfter(ctx, head[0].QueuePosition)
	mustNil(t, err)
	if len(tail) != 0 {
		t.Fatalf("cursor bypassed older obligation: %+v", tail)
	}
	_, err = s.CancelSwarmChild(ctx, p, a.RequestID, a.Revision, childAllow)
	mustNil(t, err)
	tail, err = s.QueuedSwarmChildrenAfter(ctx, head[0].QueuePosition)
	mustNil(t, err)
	if len(tail) != 1 || tail[0].Task.ID != b.ID || tail[0].QueuePosition <= head[0].QueuePosition {
		t.Fatalf("successor head: %+v", tail)
	}
	if _, err := s.QueuedSwarmChildrenAfter(ctx, -1); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("invalid private cursor: %v", err)
	}
}
