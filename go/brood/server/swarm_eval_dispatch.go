package server

import (
	"context"
	"errors"
	"reflect"

	"brood.dev/go/brood/store"
)

// replayEval разбирает текущие вопросы до обычных эффектов графа. Полномочия
// принадлежат сохранённому launcher, а не выдуманной сессии или reference human.
func (d *stepDispatcher) replayEval(ctx context.Context, swarmID string) bool {
	s := d.server
	b, err := s.store.GetSwarmEvalBinding(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		return true
	}
	requests, err := s.store.SwarmRequests(ctx, swarmID)
	if err != nil {
		return true
	}
	var requestID string
	for _, r := range requests {
		if r.State == "open" {
			requestID = r.ID
			break
		}
	}
	if requestID == "" {
		return false
	}
	scopes, err := s.evalScopes(ctx, b.CandidateSwarmID, b.ReferenceSwarmID)
	if err != nil {
		return true
	}
	actors, err := s.store.ListManagedActors(ctx, swarmID)
	if err != nil {
		return true
	}
	ids := make([]string, 0, len(actors))
	for _, a := range actors {
		ids = append(ids, a.UID)
	}
	if s.managed != nil {
		release, err := s.managed.effects.acquire(ctx, ids...)
		if err != nil {
			return true
		}
		defer release()
	} else if len(actors) > 0 {
		return true
	}
	authority := func(owner bool) store.SwarmEvalAuthorize {
		local := s.evalAuthority(scopes, nil, owner)
		return func(tx context.Context, action string, binding store.SwarmEvalBinding) error {
			current, err := s.store.ListManagedActors(tx, swarmID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(actors, current) {
				return store.ErrSwarmEvalConflict
			}
			return local(tx, action, binding)
		}
	}
	decision, err := s.store.ApplySwarmEvalReplay(ctx, store.SwarmEvalReplayInput{CandidateSwarmID: swarmID, RequestID: requestID, LauncherID: b.LauncherID}, authority(false), authority(true))
	if err != nil {
		return true
	}
	if decision.State == "applied" {
		d.kick(swarmID)
	}
	return true
}
