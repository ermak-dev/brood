package server

import (
	"context"
	"errors"
	"reflect"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// allocatedEphemeralGroup reads one consistent group snapshot. Every live job
// must already own its slot before this coordinator can issue a first Create.
func (s *Server) allocatedEphemeralGroup(ctx context.Context, intentID string) (store.EphemeralChildHistory, error) {
	var selected store.EphemeralChildHistory
	err := s.store.WithTx(ctx, func(ctx context.Context) error {
		intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
		if err != nil {
			return err
		}
		group, err := s.store.GetSwarmChildHistory(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
		if err != nil {
			return err
		}
		if group.Parent != intent.Parent || group.Interrupted || group.State == "aborted" {
			return store.ErrSwarmChildConflict
		}
		jobs, err := s.store.EphemeralChildHistoryAtStep(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
		if err != nil {
			return err
		}
		registered := make(map[string]bool, len(jobs))
		slots := len(jobs)
		for _, job := range jobs {
			if job.Intent.Parent != intent.Parent || (job.Allocation == nil && job.Failure == nil) {
				return store.ErrSwarmChildConflict
			}
			if job.ChildID != "" {
				registered[job.ChildID] = true
			}
			if job.Intent.ID == intentID {
				selected = job
			}
		}
		for _, child := range group.Children {
			if registered[child.ID] {
				continue
			}
			if child.Executor.ActorUID == "" && child.State != "cancelled" {
				return store.ErrSwarmChildConflict
			}
			slots++
		}
		if slots != group.Policy.Total || selected.Intent.ID == "" || selected.Allocation == nil {
			return store.ErrSwarmChildConflict
		}
		return nil
	})
	return selected, err
}

func (s *Server) sealAllocatedEphemeralResult(ctx context.Context, userID string, intent store.EphemeralChildIntent) error {
	swarm, err := s.store.GetSwarm(ctx, intent.Parent.Principal.SwarmID)
	if err != nil {
		return err
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		return err
	}
	return s.store.SealEphemeralChildResult(ctx, intent.ID, func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if a.Action != "seal_ephemeral_result" || a.Parent != intent.Parent {
			return store.ErrSwarmChildDenied
		}
		if err := s.authorizeSwarmOwnerIntent(ctx, userID, intent.Parent.Principal.SwarmID); err != nil {
			return err
		}
		if err := s.managed.childAuthority(ctx, a); err != nil {
			return err
		}
		origin, err := s.store.ProjectRepository(ctx, intent.Parent.Principal.ProjectID)
		if err != nil {
			return err
		}
		if origin != intent.Origin {
			return store.ErrPipelineSourceUnverified
		}
		current, err := s.store.GetSwarm(ctx, swarm.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, swarm) {
			return store.ErrSwarmChildConflict
		}
		return nil
	})
}

// advanceAllocatedEphemeralChild performs at most one granted transition. It is
// deliberately not a background service: admission must reserve the entire group
// before scheduling this coordinator. Observing a requested task grants no retry.
func (s *Server) advanceAllocatedEphemeralChild(ctx context.Context, intentID string) error {
	job, err := s.allocatedEphemeralGroup(ctx, intentID)
	if err != nil {
		return err
	}
	actor, err := s.store.GetManagedActor(ctx, job.Allocation.ActorUID)
	if err != nil {
		return err
	}
	if actor.Incarnation != job.Allocation.ActorIncarnation {
		return store.ErrSwarmChildConflict
	}
	parent, err := s.store.GetManagedActor(ctx, job.Intent.Parent.Principal.ActorUID)
	if err != nil {
		return err
	}
	if actor.Phase == "deleted" {
		return nil
	}
	if _, err := s.ownerActorOperationOrigin(ctx, actor); err == nil {
		return store.ErrSwarmChildConflict
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if job.ChildID == "" && job.Failure == nil && ephemeralTerminalTaskFailure(actor) {
		return s.recordAllocatedEphemeralTaskFailure(ctx, parent.DelegatorID, job, actor)
	}
	if (job.Failure != nil || job.ExecutorFailure != nil) && actor.Operation != "delete" {
		_, err = s.beginEphemeralChildLifecycle(ctx, parent.DelegatorID, job.Intent.Parent, intentID, "cleanup")
		return err
	}
	if job.ChildID != "" && actor.Operation != "delete" {
		work, err := s.store.SwarmChildWorkByID(ctx, job.ChildID)
		if err != nil {
			return err
		}
		if work.Task.State == "pending" || work.Task.State == "running" {
			stopped, err := s.observeRegisteredEphemeralWorker(ctx, parent.DelegatorID, job, actor, work.Task)
			if err != nil || stopped {
				return err
			}
		}
		if work.Task.State == "pending" {
			_, err = s.prepareEphemeralChildDelivery(ctx, parent.DelegatorID, job.Intent.Parent, intentID)
			return err
		}
		if work.Task.State == "cancelled" {
			_, err = s.beginEphemeralChildLifecycle(ctx, parent.DelegatorID, job.Intent.Parent, intentID, "cleanup")
			return err
		}
		if work.Task.State == "completed" {
			if job.ResultSeal == nil {
				return s.sealAllocatedEphemeralResult(ctx, parent.DelegatorID, job.Intent)
			}
			_, err = s.beginEphemeralChildLifecycle(ctx, parent.DelegatorID, job.Intent.Parent, intentID, "cleanup")
			return err
		}
		return nil
	}
	if actor.OperationID == job.Allocation.CreateOperationID {
		if job.CreateGrant != nil && job.CreateGrant.State == "queued" {
			_, err = s.createAllocatedEphemeralChild(ctx, parent.DelegatorID, job.Intent.Parent, intentID)
			return err
		}
		if actor.Stage == "done" && actor.Phase == "stopped" {
			_, err = s.beginEphemeralChildLifecycle(ctx, parent.DelegatorID, job.Intent.Parent, intentID, "start")
			return err
		}
		return nil
	}
	history, err := s.store.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
	if err != nil {
		return err
	}
	if history.IntentID != intentID {
		return store.ErrSwarmChildConflict
	}
	if history.Kind == "start" && actor.Stage == "done" && actor.Phase == "running" {
		if job.Attestation != nil {
			if job.Attestation.State == "attested" {
				_, err = s.registerEphemeralChild(ctx, parent.DelegatorID, job.Intent.Parent, intentID)
				return err
			}
			return nil
		}
		attemptID, err := newPipelineCompletionNonce()
		if err != nil {
			return err
		}
		_, err = s.attestEphemeralChild(ctx, parent.DelegatorID, job.Intent.Parent, intentID, attemptID)
		return err
	}
	if stopNotRequested(actor) || deleteNotRequested(actor) {
		_, err = s.completeEphemeralChildCleanup(ctx, parent.DelegatorID, job.Intent.Parent, actor.UID, actor.Incarnation, actor.OperationID, actor.Revision)
		if !errors.Is(err, proxmox.ErrOwnership) {
			return err
		}
	}
	if startNotRequested(actor) || stopNotRequested(actor) || deleteNotRequested(actor) {
		_, err = s.submitEphemeralChildLifecycleStage(ctx, parent.DelegatorID, job.Intent.Parent, actor.UID, actor.Incarnation, actor.OperationID, actor.Stage, actor.Revision)
		return err
	}
	if ephemeralCleanupLifecycle(history.Kind) && (actor.Stage == "stop" || actor.Stage == "delete") && actor.UPID != "" {
		_, err = s.completeEphemeralChildCleanup(ctx, parent.DelegatorID, job.Intent.Parent, actor.UID, actor.Incarnation, actor.OperationID, actor.Revision)
		if actor.Stage == "stop" && errors.Is(err, proxmox.ErrOwnership) {
			return nil
		}
		return err
	}
	return nil
}
