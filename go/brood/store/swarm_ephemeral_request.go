package store

import (
	"context"
	"errors"
	"fmt"
)

// OpenEphemeralChildRequest opens an actor question only within its exact running,
// proof-bound ephemeral child. The trusted caller authenticates the executor;
// authorize is mandatory, local-only and receives the transaction context with
// action ask_ephemeral_operator and Caller equal to the executor. Question and
// immutable parent/pass context commit together. This does not complete work,
// grant model authority or change instruction/answer delivery permissions.
func (s *Store) OpenEphemeralChildRequest(ctx context.Context, childID string, in ActorRequestInput, authorize SwarmChildAuthorize) (SwarmRequest, error) {
	if !validPipelineField(childID) || authorize == nil {
		return SwarmRequest{}, ErrSwarmRequestDenied
	}
	return s.openActorRequest(ctx, in, childID, authorize)
}

func (s *Store) ephemeralQuestionParent(ctx context.Context, childID string, in ActorRequestInput, action string, authorize SwarmChildAuthorize) (SwarmChildParent, error) {
	intent, err := s.GetEphemeralChildIntentForTask(ctx, childID)
	if err != nil {
		return SwarmChildParent{}, err
	}
	intent, allocation, err := s.currentRegisteredEphemeralScope(ctx, intent.ID, action, func(ctx context.Context, a SwarmChildAuthorization) error {
		a.Caller = a.Executor
		return authorize(ctx, a)
	})
	if err != nil {
		return SwarmChildParent{}, err
	}
	if in.SwarmID != intent.Parent.Principal.SwarmID || in.ActorUID != allocation.ActorUID || in.ActorIncarnation != allocation.ActorIncarnation {
		return SwarmChildParent{}, ErrSwarmRequestDenied
	}
	work, err := s.SwarmChildWorkByID(ctx, childID)
	if err != nil {
		return SwarmChildParent{}, err
	}
	if work.Parent != intent.Parent || work.Task.State != "running" || work.Task.Executor.ActorUID != in.ActorUID || work.Task.Executor.Incarnation != in.ActorIncarnation {
		return SwarmChildParent{}, ErrSwarmRequestDenied
	}
	var intervened bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE (actor_uid=? AND incarnation=?) OR (actor_uid=? AND incarnation=?))`, in.ActorUID, in.ActorIncarnation, intent.Parent.Principal.ActorUID, intent.Parent.Principal.Incarnation).Scan(&intervened); err != nil {
		return SwarmChildParent{}, err
	}
	if intervened {
		return SwarmChildParent{}, ErrSwarmRequestDenied
	}
	if _, err := s.CurrentActorOperation(ctx, intent.Parent.Principal.ActorUID); err == nil {
		return SwarmChildParent{}, ErrSwarmRequestDenied
	} else if !errors.Is(err, ErrNotFound) {
		return SwarmChildParent{}, err
	}
	return intent.Parent, nil
}

func (s *Store) captureEphemeralQuestionContext(ctx context.Context, requestID string, parent SwarmChildParent) error {
	result, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_request_pass_contexts(request_id,swarm_id,pass_sequence,observed_sequence) VALUES(?,?,?,?)`, requestID, parent.Principal.SwarmID, parent.Pass, parent.Sequence)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("ephemeral question context was not retained")
	}
	return nil
}
