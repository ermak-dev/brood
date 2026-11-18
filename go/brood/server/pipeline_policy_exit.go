package server

import (
	"context"
	"errors"
	"log/slog"

	"brood.dev/go/brood/store"
)

// pendingPolicyResult выбирает также известный отказ root observation. Unknown
// остаётся held: его нельзя ни разрешить как review, ни повторить как result RPC.
func (d *stepDispatcher) pendingPolicyResult(ctx context.Context, swarmID string) (store.PipelineCompletionIntent, bool, error) {
	s := d.server.store
	intent, err := s.PendingObservedPipelineIntent(ctx, swarmID)
	if !errors.Is(err, store.ErrNotFound) {
		return intent, false, err
	}
	steps, err := s.SwarmSteps(ctx, swarmID)
	if err != nil {
		return intent, false, err
	}
	if len(steps) == 0 {
		return intent, false, store.ErrNotFound
	}
	last := steps[len(steps)-1]
	if last.State != "inflight" && last.State != "delivering" && last.State != "unconfirmed" {
		return intent, false, store.ErrNotFound
	}
	observation, err := s.GetPipelineResultObservation(ctx, swarmID, last.Sequence)
	if err != nil {
		return intent, false, err
	}
	if observation.State != "refused" {
		return intent, false, store.ErrNotFound
	}
	return observation.Intent, true, nil
}

func (d *stepDispatcher) resolvePolicyResult(ctx context.Context, swarmID string) bool {
	s := d.server
	intent, refused, err := d.pendingPolicyResult(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		return true
	}
	actor, err := s.store.GetManagedActor(ctx, intent.ActorUID)
	if err != nil {
		return true
	}
	actor, namespace, err := s.managed.authorizeCommandActor(ctx, actor)
	if err != nil {
		return true
	}
	actorApplied := false
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		if err := s.managed.checkResultCollectorAuthority(ctx, actor, intent, namespace.AtespaceName, namespace.AtespaceUID); err != nil {
			return err
		}
		if refused {
			// Известный root refusal — не actor outcome и не разрешение обычного
			// перехода. Только Store решает, является ли это terminal inline review.
			_, err := s.store.ResolvePipelinePolicyReview(ctx, swarmID, intent.Sequence)
			return err
		}
		_, err := s.store.ResolvePipelinePolicyExit(ctx, swarmID, intent.Sequence)
		if errors.Is(err, store.ErrNoPolicyExit) {
			_, err = s.store.ResolvePipelinePolicyHuman(ctx, swarmID, intent.Sequence)
		}
		if errors.Is(err, store.ErrNoPolicyHuman) {
			var resolution store.PipelinePolicyTransitionResolution
			resolution, err = s.store.ResolvePipelinePolicyActor(ctx, swarmID, intent.Sequence)
			actorApplied = resolution.Applied && !resolution.Rejected
		}
		return err
	})
	if errors.Is(err, store.ErrNoPolicyActor) {
		return false
	}
	if err == nil && actorApplied {
		d.kick(swarmID)
	}
	if err != nil && ctx.Err() == nil {
		slog.Info("policy result bookkeeping held", "swarm", swarmID, "err", err.Error())
	}
	return true
}
