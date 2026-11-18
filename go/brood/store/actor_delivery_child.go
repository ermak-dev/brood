package store

import (
	"context"
	"errors"
)

// PrepareEphemeralChildDelivery atomically pins one instruction and starts its
// exact registered child in FULL sync. SourceID is the child task ID, RequestID
// the immutable intent key and Commit its attested source. Payload is the trusted
// server-built message, retained byte-for-byte. Explicit current authority is
// required even on retries. No transport attempt or child completion is granted.
func (s *Store) PrepareEphemeralChildDelivery(ctx context.Context, intentID string, in ActorDeliveryInput, authorize SwarmChildAuthorize) (ActorDeliveryIntent, error) {
	var out ActorDeliveryIntent
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		if !validActorDeliveryInput(in) || in.SourceKind != "child" {
			return ErrActorDeliveryConflict
		}
		child, err := s.currentEphemeralDeliveryChild(ctx, intentID, in, authorize)
		if err != nil {
			return err
		}
		old, err := s.GetActorDeliveryForSource(ctx, in.SwarmID, "child", in.SourceID)
		if err == nil {
			if old.Input != in || child.State != "running" {
				return ErrActorDeliveryConflict
			}
			out = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if child.State != "pending" || child.Revision != 1 {
			return ErrActorDeliveryConflict
		}
		if err := s.guardChildAllocation(ctx, child.Executor.ActorUID); err != nil {
			return err
		}
		out, err = s.insertActorDelivery(ctx, in, false)
		if err != nil {
			return err
		}
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_child_tasks SET state='running',revision=revision+1 WHERE id=? AND state='pending' AND revision=? AND role='' AND delivery=''`, child.ID, child.Revision)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrActorDeliveryConflict
		}
		return nil
	})
	if err != nil {
		return ActorDeliveryIntent{}, err
	}
	return out, nil
}

func (s *Store) currentEphemeralDeliveryChild(ctx context.Context, intentID string, in ActorDeliveryInput, authorize SwarmChildAuthorize) (SwarmChildTask, error) {
	intent, allocation, err := s.currentRegisteredEphemeralScope(ctx, intentID, "prepare_ephemeral_delivery", authorize)
	if err != nil {
		return SwarmChildTask{}, err
	}
	return s.matchEphemeralDeliveryChild(ctx, intent, allocation, in)
}

func (s *Store) matchEphemeralDeliveryChild(ctx context.Context, intent EphemeralChildIntent, allocation EphemeralChildAllocation, in ActorDeliveryInput) (SwarmChildTask, error) {
	if in.SwarmID != intent.Parent.Principal.SwarmID || in.RequestID != intent.RequestID || in.Commit != intent.SourceCommit || in.ActorUID != allocation.ActorUID || in.Incarnation != allocation.ActorIncarnation || in.AtespaceUID != intent.Parent.Principal.AtespaceUID {
		return SwarmChildTask{}, ErrActorDeliveryConflict
	}
	join, err := s.loadChildJoin(ctx, intent.Parent)
	if err != nil {
		return SwarmChildTask{}, err
	}
	executor := intent.Parent.Principal
	executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
	for _, child := range join.Children {
		if child.ID == in.SourceID && child.RequestID == intent.RequestID && child.Payload == intent.Payload && child.Executor == executor && child.Role == "" && child.Delivery == "" && child.DeliveryDetail == "" && child.Outcome == "" && child.Report == "" && child.FinishedAt == "" && (child.State == "pending" || child.State == "running") {
			return child, nil
		}
	}
	return SwarmChildTask{}, ErrActorDeliveryConflict
}

// Транспортные операции вызывают тот же current scope, но их собственный
// обязательный callback проверяет текущую authority внутри своей транзакции.
func (s *Store) ephemeralDeliveryCurrent(ctx context.Context, in ActorDeliveryInput) (bool, error) {
	intent, err := s.GetEphemeralChildIntentForTask(ctx, in.SourceID)
	if err != nil {
		return false, err
	}
	if _, err := s.GetEphemeralChildExecutorFailure(ctx, intent.ID); err == nil {
		return false, ErrActorDeliveryConflict
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	allocation, err := s.GetEphemeralChildAllocation(ctx, intent.ID)
	if err != nil {
		return false, err
	}
	if err := s.checkEphemeralChildScope(ctx, intent, allocation); err != nil {
		return false, err
	}
	if err := s.checkRegisteredEphemeralProof(ctx, intent, allocation); err != nil {
		return false, err
	}
	child, err := s.matchEphemeralDeliveryChild(ctx, intent, allocation, in)
	if err != nil {
		return false, err
	}
	if child.State != "running" {
		return false, ErrActorDeliveryConflict
	}
	return false, nil
}
