package server

import (
	"context"
	"time"
)

func (s *Server) stopEphemeralChildRecovery() {
	if s.ephemeralRecoveryClose != nil {
		s.ephemeralRecoveryClose()
	}
}

// startEphemeralChildRecovery owns one bounded serial loop. Allocated work runs
// before admission so verified cleanup can return capacity to waiting groups.
// The returned stop function waits for in-flight bookkeeping before Store closes.
func (s *Server) startEphemeralChildRecovery(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var allocatedAfter, queuedAfter int64
		var cursors map[string]int
		for ctx.Err() == nil {
			if s.managed != nil {
				pass, finish := context.WithTimeout(ctx, s.managed.actorPassTimeout())
				allocatedAfter, _ = s.recoverAllocatedEphemeralChildren(pass, allocatedAfter, 8)
				if pass.Err() == nil {
					queuedAfter, cursors, _ = s.recoverQueuedEphemeralGroups(pass, queuedAfter, 4, cursors, 128)
				}
				finish()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
