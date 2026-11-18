package server

import (
	"context"
	"errors"

	"brood.dev/go/brood/store"
)

// recoverQueuedEphemeralGroups admits complete groups in FIFO order without
// physical effects. Missing capacity remains queued. A held group consumes this
// cycle's budget but not later cycles or another group's place in the scan.
func (s *Server) recoverQueuedEphemeralGroups(ctx context.Context, after int64, budget int, cursors map[string]int, maxChecks int) (int64, map[string]int, error) {
	if budget < 1 || budget > 128 || maxChecks < 1 || maxChecks > 128 {
		return after, cursors, store.ErrSwarmChildConflict
	}
	jobs, err := s.store.QueuedEphemeralChildIntents(ctx, after)
	if err != nil {
		return after, cursors, err
	}
	if len(jobs) == 0 {
		return 0, cursors, nil
	}
	seen := map[store.SwarmChildParent]bool{}
	var failures error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return after, cursors, errors.Join(failures, err)
		}
		if !seen[job.Parent] && len(seen) >= budget {
			break
		}
		after = job.QueuePosition
		if seen[job.Parent] {
			continue
		}
		seen[job.Parent] = true
		_, next, err := s.allocateQueuedEphemeralGroup(ctx, job.ID, cursors, maxChecks)
		if next != nil {
			cursors = next
		}
		failures = errors.Join(failures, err)
	}
	return after, cursors, failures
}

// recoverAllocatedEphemeralChildren advances a bounded FIFO page once. The cursor
// advances even for held jobs so one revoked or uncertain job cannot starve later
// groups. A new cycle rechecks them; it never resets their retained effect grants.
func (s *Server) recoverAllocatedEphemeralChildren(ctx context.Context, after int64, budget int) (int64, error) {
	if budget < 1 || budget > 128 {
		return after, store.ErrSwarmChildConflict
	}
	jobs, err := s.store.AllocatedEphemeralChildIntents(ctx, after)
	if err != nil {
		return after, err
	}
	if len(jobs) == 0 {
		return 0, nil
	}
	var failures error
	for _, job := range jobs[:min(len(jobs), budget)] {
		if err := ctx.Err(); err != nil {
			return after, errors.Join(failures, err)
		}
		after = job.QueuePosition
		failures = errors.Join(failures, s.advanceAllocatedEphemeralChild(ctx, job.ID))
	}
	return after, failures
}
