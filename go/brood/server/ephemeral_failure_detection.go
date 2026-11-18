package server

import (
	"context"
	"errors"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// observeRegisteredEphemeralWorker is scheduled by allocated recovery even after
// the original Start settles. Only a fresh owned stopped observation is a logical
// failure; neither a missing session nor an absent container proves this fact.
func (s *Server) observeRegisteredEphemeralWorker(ctx context.Context, userID string, job store.EphemeralChildHistory, actor store.ManagedActor, child store.SwarmChildTask) (bool, error) {
	if s.managed == nil || job.Allocation == nil || job.ChildID != child.ID {
		return false, store.ErrSwarmChildConflict
	}
	if actor.Operation != "start" || actor.Stage != "done" || actor.Desired != "running" || actor.UPID == "" {
		return false, nil
	}
	// Несвязанная операция не становится новым failure producer и не меняет
	// существующие правила подготовки доставки по retained proof.
	history, err := s.store.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if history.Kind != "start" || history.IntentID != job.Intent.ID {
		return false, nil
	}
	m := s.managed
	ctx, cancel := context.WithTimeout(ctx, m.actorPassTimeout())
	defer cancel()
	current, pool, authorize, err := s.ephemeralLifecyclePreflight(ctx, userID, job.Intent.Parent, job.Intent.ID, "start")
	if err != nil {
		return false, err
	}
	if current != actor {
		return false, store.ErrRevisionConflict
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	release, err := m.effects.acquire(ctx, job.Intent.Parent.Principal.ActorUID, actor.UID)
	if err != nil {
		return false, err
	}
	defer release()
	check := func(ctx context.Context) error {
		current, err := s.store.GetManagedActor(ctx, actor.UID)
		if err != nil {
			return err
		}
		if current != actor {
			return store.ErrRevisionConflict
		}
		// Отсутствие manual provenance не доказывает automatic Start.
		history, err := s.store.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
		if err != nil {
			return err
		}
		if history.Kind != "start" || history.IntentID != job.Intent.ID || history.ActorUID != actor.UID || history.ActorIncarnation != actor.Incarnation || history.OperationID != actor.OperationID {
			return store.ErrSwarmChildConflict
		}
		if _, err := s.ownerActorOperationOrigin(ctx, actor); err == nil {
			return store.ErrSwarmChildConflict
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		work, err := s.store.SwarmChildWorkByID(ctx, child.ID)
		if err != nil {
			return err
		}
		if work.Parent != job.Intent.Parent || work.Task != child {
			return store.ErrSwarmChildConflict
		}
		return s.store.CheckEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID, authorize)
	}
	if err := check(ctx); err != nil {
		return false, err
	}
	client, err := m.client(pool)
	if err != nil {
		return false, err
	}
	// Не используем absence shortcut: нужен положительный exact ownership proof.
	if _, err := inspectManaged(ctx, client, actor); err != nil {
		return false, err
	}
	status, err := client.Status(ctx, actor.CTID)
	if err != nil {
		return false, err
	}
	if status.Status == "running" {
		return false, nil
	}
	if status.Status != "stopped" {
		return false, proxmox.ErrUncertain
	}
	err = s.store.RecordEphemeralChildExecutorFailure(ctx, job.Intent.ID, "Registered worker container was observed stopped with verified ownership.", func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if a.Action != "fail_ephemeral_executor" || a.Parent != job.Intent.Parent {
			return store.ErrSwarmChildDenied
		}
		return check(ctx)
	})
	return true, err
}

// ephemeralTerminalTaskFailure accepts only the receipt retained by the task
// observer. A diagnostic without a bound, completed operation is not evidence.
func ephemeralTerminalTaskFailure(actor store.ManagedActor) bool {
	return actor.Stage == "done" && actor.Phase == "error" && actor.UPID != "" && actor.Error == managedProviderError(proxmox.ErrTaskFailed) && actor.Desired == "running"
}

// recordAllocatedEphemeralTaskFailure is a separate logical producer, not an
// observer or cleanup grant. It retains the original receipt and physical hold.
func (s *Server) recordAllocatedEphemeralTaskFailure(ctx context.Context, userID string, job store.EphemeralChildHistory, actor store.ManagedActor) error {
	if s.managed == nil || job.Allocation == nil || job.ChildID != "" || !ephemeralTerminalTaskFailure(actor) {
		return store.ErrSwarmChildConflict
	}
	ctx, cancel := context.WithTimeout(ctx, s.managed.actorPassTimeout())
	defer cancel()
	current, _, authorize, err := s.ephemeralLifecyclePreflight(ctx, userID, job.Intent.Parent, job.Intent.ID, "start")
	if err != nil {
		return err
	}
	if current != actor {
		return store.ErrRevisionConflict
	}
	// Как у effect adapters: lifecycle, затем sorted parent/executor gates.
	s.managed.lifecycle.Lock()
	defer s.managed.lifecycle.Unlock()
	release, err := s.managed.effects.acquire(ctx, job.Intent.Parent.Principal.ActorUID, actor.UID)
	if err != nil {
		return err
	}
	defer release()
	return s.store.RecordEphemeralChildFailure(ctx, job.Intent.ID, "Original worker provisioning task failed before child registration.", func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if a.Action != "fail_ephemeral" || a.Parent != job.Intent.Parent {
			return store.ErrSwarmChildDenied
		}
		// Preflight callback содержит только local authority/source/pool/scope reads.
		a.Action = "begin_ephemeral_start"
		if err := authorize(ctx, a); err != nil {
			return err
		}
		current, err := s.store.GetManagedActor(ctx, actor.UID)
		if err != nil {
			return err
		}
		if current != actor || !ephemeralTerminalTaskFailure(current) {
			return store.ErrRevisionConflict
		}
		pool, err := s.store.GetManagedPool(ctx, job.Intent.PoolUID)
		if err != nil {
			return err
		}
		if pool.State != "enabled" || pool.UID != actor.PoolUID || pool.Template != job.Intent.Template || pool.SourceFingerprint != job.Intent.SourceFingerprint || pool.Source.ClusterIdentity != actor.ClusterIdentity {
			return store.ErrManagedBinding
		}
		allocation, err := s.store.GetEphemeralChildAllocation(ctx, job.Intent.ID)
		if err != nil {
			return err
		}
		if allocation != *job.Allocation || allocation.ActorUID != actor.UID || allocation.ActorIncarnation != actor.Incarnation {
			return store.ErrSwarmChildConflict
		}
		if _, err := s.ownerActorOperationOrigin(ctx, actor); err == nil {
			return store.ErrSwarmChildConflict
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// Не превращаем незавершённый workspace operation в logical failure.
		if _, err := s.store.CurrentActorOperation(ctx, actor.UID); err == nil {
			return store.ErrSwarmChildConflict
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		switch actor.Operation {
		case "create":
			if actor.OperationID != allocation.CreateOperationID {
				return store.ErrSwarmChildConflict
			}
			queued, err := s.managed.ephemeralCreateQueued(ctx, actor)
			if err != nil {
				return err
			}
			if queued {
				return store.ErrSwarmChildConflict
			}
		case "start":
			history, err := s.store.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
			if err != nil {
				return err
			}
			if history.IntentID != job.Intent.ID || history.Kind != "start" || history.ActorUID != actor.UID || history.ActorIncarnation != actor.Incarnation || history.OperationID != actor.OperationID {
				return store.ErrSwarmChildConflict
			}
		default:
			return store.ErrSwarmChildConflict
		}
		return nil
	})
}
