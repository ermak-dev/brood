package store

import "context"

// Положительный pending(revision=1)→cancelled(revision=2) CAS исключает Start,
// Prepare и legacy transport. Отсутствие delivery само по себе не доказательство:
// обязательны точная registered membership, terminal evidence и attested proof.
func (s *Store) cancelCleanupSource(ctx context.Context, id string) (bool, error) {
	var cancelled bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ephemeral_child_intents i JOIN swarm_child_tasks c ON c.swarm_id=i.swarm_id AND c.parent_sequence=i.parent_sequence AND c.request_id=i.request_id WHERE i.id=? AND c.state='cancelled')`, id).Scan(&cancelled)
	return cancelled, err
}

func (s *Store) cancelCleanupEvidence(ctx context.Context, intent EphemeralChildIntent, allocation EphemeralChildAllocation, join SwarmChildJoin) error {
	executor := intent.Parent.Principal
	executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
	for _, child := range join.Children {
		if child.RequestID != intent.RequestID {
			continue
		}
		membership, err := s.GetEphemeralChildIntentForTask(ctx, child.ID)
		if err != nil {
			return err
		}
		if membership != intent || child.Executor != executor || child.Payload != intent.Payload || child.Role != "" || child.Delivery != "" || child.DeliveryDetail != "" || child.State != "cancelled" || child.Revision != 2 || child.FinishedAt == "" || child.CreatedAt == "" || child.Report != "" || child.Outcome != "" {
			return ErrSwarmChildConflict
		}
		var clean bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT `+ephemeralCancelNoIndependentEvidence, child.ID, intent.ID, intent.ID, intent.ID).Scan(&clean); err != nil {
			return err
		}
		if !clean {
			return ErrSwarmChildConflict
		}
		proof, err := s.GetEphemeralChildAttestation(ctx, intent.ID)
		if err != nil {
			return err
		}
		if proof.State != "attested" || proof.ActorUID != executor.ActorUID || proof.ActorIncarnation != executor.Incarnation || proof.Origin != intent.Origin || proof.Commit != intent.SourceCommit || proof.AttestedOrigin != intent.Origin || proof.AttestedCommit != intent.SourceCommit {
			return ErrPipelineSourceUnverified
		}
		return nil
	}
	return ErrSwarmChildConflict
}

// Все attempts ссылаются на immutable delivery; запрещаем даже prepared/probe-only
// record без попыток. Не принимаем not_sent за отсутствие issued instruction.
const ephemeralCancelNoIndependentEvidence = `NOT EXISTS(SELECT 1 FROM actor_deliveries WHERE source_kind='child' AND source_id=?)
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_result_seals WHERE intent_id=?)
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_failures WHERE intent_id=?)
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_executor_failures WHERE intent_id=?)`
