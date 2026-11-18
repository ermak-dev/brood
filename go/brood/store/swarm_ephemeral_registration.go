package store

import (
	"context"
	"errors"
)

// RegisterEphemeralChild commits a pending child in FULL sync only after retained
// executor attestation. Parent, request, payload and executor are derived solely
// from immutable intent and allocation. Explicit current authority is required,
// including on exact read-only retries. All nested transactions are rejected.
// This does not deliver instructions, bind a workflow role or start the child.
func (s *Store) RegisterEphemeralChild(ctx context.Context, intentID string, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	if !validPipelineField(intentID) {
		return SwarmChildTask{}, ErrSwarmChildConflict
	}
	var result SwarmChildTask
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		intent, allocation, err := s.currentRegisteredEphemeralScope(ctx, intentID, "register_ephemeral", authorize)
		if err != nil {
			return err
		}
		executor := intent.Parent.Principal
		executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
		join, err := s.loadChildJoin(ctx, intent.Parent)
		if err != nil {
			return err
		}
		for _, old := range join.Children {
			if old.RequestID == intent.RequestID {
				if old.Executor != executor || old.Payload != intent.Payload || old.Role != "" || old.Delivery != "" {
					return ErrSwarmChildConflict
				}
				result = old
				return nil
			}
		}
		result = SwarmChildTask{ID: NewID(), RequestID: intent.RequestID, Payload: intent.Payload, Executor: executor, State: "pending", Revision: 1, CreatedAt: nowRFC3339()}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_child_tasks(id,swarm_id,parent_sequence,request_id,payload,actor_uid,actor_incarnation,state,revision,created_at,role,delivery) VALUES(?,?,?,?,?,?,?,'pending',1,?,'','')`, result.ID, intent.Parent.Principal.SwarmID, intent.Parent.Sequence, result.RequestID, result.Payload, executor.ActorUID, executor.Incarnation, result.CreatedAt)
		return ephemeralSlotError(err)
	})
	if err != nil {
		return SwarmChildTask{}, err
	}
	return result, nil
}

func (s *Store) currentRegisteredEphemeralScope(ctx context.Context, intentID, action string, authorize SwarmChildAuthorize) (EphemeralChildIntent, EphemeralChildAllocation, error) {
	intent, allocation, err := s.currentEphemeralChildScope(ctx, intentID, action, true, authorize)
	if err != nil {
		return intent, allocation, err
	}
	return intent, allocation, s.checkRegisteredEphemeralProof(ctx, intent, allocation)
}

func (s *Store) checkRegisteredEphemeralProof(ctx context.Context, intent EphemeralChildIntent, allocation EphemeralChildAllocation) error {
	proof, err := s.ephemeralChildAttestation(ctx, intent.ID)
	if err != nil {
		return err
	}
	if proof.State != "attested" || proof.ActorUID != allocation.ActorUID || proof.ActorIncarnation != allocation.ActorIncarnation || proof.Origin != intent.Origin || proof.Commit != intent.SourceCommit || proof.AttestedOrigin != intent.Origin || proof.AttestedCommit != intent.SourceCommit {
		return ErrSwarmChildConflict
	}
	pool, err := s.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		return err
	}
	if pool.State != "enabled" || pool.SourceFingerprint != intent.SourceFingerprint || pool.Template != intent.Template {
		return ErrManagedBinding
	}
	if _, err := s.CurrentActorOperation(ctx, allocation.ActorUID); err == nil {
		return ErrSwarmChildConflict
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	// Даже завершённое вмешательство не восстанавливает доверие к старому workspace.
	var intervened bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE actor_uid=? AND incarnation=?)`, allocation.ActorUID, allocation.ActorIncarnation).Scan(&intervened); err != nil {
		return err
	}
	if intervened {
		return ErrSwarmChildConflict
	}
	return nil
}

func (s *Store) unregisteredEphemeralChildCount(ctx context.Context, p SwarmChildParent) (int, error) {
	var queued int
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*) FROM ephemeral_child_intents e WHERE e.swarm_id=? AND e.parent_sequence=? AND NOT EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=e.swarm_id AND c.parent_sequence=e.parent_sequence AND c.request_id=e.request_id)`, p.Principal.SwarmID, p.Sequence).Scan(&queued)
	return queued, err
}
