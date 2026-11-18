package store

import (
	"context"
	"database/sql"
	"errors"
)

// EphemeralChildInventory is an exact-incarnation current inventory snapshot,
// not historical proof of creation, completion or deletion. Physical credentials,
// ownership nonce, CTID and provider task identifiers are deliberately absent.
type EphemeralChildInventory struct {
	UID, Name, Phase, Desired, Operation, Stage, UpdatedAt string
	Incarnation                                            int64
}

// EphemeralChildHistory keeps queued work, allocation, attestation, registered
// execution and retained result evidence distinct. It is internal read-only
// history and contains the original task; adapters enforce visibility and must
// project explicit public fields, never use this as a bootstrap or authority DTO.
type EphemeralChildHistory struct {
	Intent               EphemeralChildIntent
	Allocation           *EphemeralChildAllocation
	CreateGrant          *EphemeralChildCreateGrant
	Attestation          *EphemeralChildAttestation
	ChildID              string
	Inventory            *EphemeralChildInventory
	InventoryUnavailable string
	ResultSeal           *EphemeralChildResultSeal
	Failure              *EphemeralChildFailure
	ExecutorFailure      *EphemeralChildExecutorFailure
}

// GetEphemeralChildHistory reads one consistent immutable job history including
// before allocation/proof and after inventory removal. No current scheduling
// authority or effect permission is established, and no missing fact is invented.
func (s *Store) GetEphemeralChildHistory(ctx context.Context, intentID string) (EphemeralChildHistory, error) {
	var result EphemeralChildHistory
	err := s.WithTx(ctx, func(ctx context.Context) error {
		intent, err := s.GetEphemeralChildIntent(ctx, intentID)
		if err != nil {
			return err
		}
		result, err = s.ephemeralChildHistory(ctx, intent)
		return err
	})
	if err != nil {
		return EphemeralChildHistory{}, err
	}
	return result, nil
}

// EphemeralChildHistoryAtStep reads at most the bounded membership of one exact
// parent sequence, in original queue order. Allocated and registered intents stay
// present. The caller separately enforces swarm visibility; reading starts nothing.
func (s *Store) EphemeralChildHistoryAtStep(ctx context.Context, swarmID string, sequence int64) ([]EphemeralChildHistory, error) {
	if !validPipelineField(swarmID) || sequence < 1 {
		return nil, ErrNotFound
	}
	result := []EphemeralChildHistory{}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		intents, err := s.ephemeralChildIntentsAtStep(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		for _, intent := range intents {
			history, err := s.ephemeralChildHistory(ctx, intent)
			if err != nil {
				return err
			}
			result = append(result, history)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ephemeralChildIntentsAtStep(ctx context.Context, swarmID string, sequence int64) ([]EphemeralChildIntent, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE swarm_id=? AND parent_sequence=? ORDER BY rowid LIMIT ?`, swarmID, sequence, MaxSwarmChildren+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	intents := []EphemeralChildIntent{}
	for rows.Next() {
		if len(intents) == MaxSwarmChildren {
			return nil, ErrPassTraceLimit
		}
		intent, err := scanEphemeralChildIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *Store) ephemeralChildHistory(ctx context.Context, intent EphemeralChildIntent) (EphemeralChildHistory, error) {
	result := EphemeralChildHistory{Intent: intent}
	failure, err := s.GetEphemeralChildFailure(ctx, intent.ID)
	if err == nil {
		result.Failure = &failure
	} else if !errors.Is(err, ErrNotFound) {
		return EphemeralChildHistory{}, err
	}
	allocation, err := s.GetEphemeralChildAllocation(ctx, intent.ID)
	if errors.Is(err, ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return EphemeralChildHistory{}, err
	}
	result.Allocation = &allocation
	grant, err := s.GetEphemeralChildCreateGrant(ctx, intent.ID)
	if err == nil {
		result.CreateGrant = &grant
	} else if !errors.Is(err, ErrNotFound) {
		return EphemeralChildHistory{}, err
	}
	proof, err := s.GetEphemeralChildAttestation(ctx, intent.ID)
	if err == nil {
		result.Attestation = &proof
	} else if !errors.Is(err, ErrNotFound) {
		return EphemeralChildHistory{}, err
	}
	actor, err := s.GetManagedActor(ctx, allocation.ActorUID)
	switch {
	case errors.Is(err, ErrNotFound):
		result.InventoryUnavailable = "not_retained"
	case err != nil:
		return EphemeralChildHistory{}, err
	case actor.Incarnation != allocation.ActorIncarnation:
		result.InventoryUnavailable = "incarnation_changed"
	case actor.Class != ActorClassEphemeral || actor.SwarmID != intent.Parent.Principal.SwarmID || actor.AtespaceUID != intent.Parent.Principal.AtespaceUID || actor.ParentUID != intent.Parent.Principal.ActorUID || actor.PoolUID != intent.PoolUID || actor.Template != intent.Template || actor.SourceFingerprint != intent.SourceFingerprint || actor.Origin != intent.Origin:
		result.InventoryUnavailable = "scope_changed"
	default:
		result.Inventory = &EphemeralChildInventory{UID: actor.UID, Name: actor.Name, Incarnation: actor.Incarnation, Phase: actor.Phase, Desired: actor.Desired, Operation: actor.Operation, Stage: actor.Stage, UpdatedAt: actor.UpdatedAt}
	}
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT id FROM swarm_child_tasks WHERE swarm_id=? AND parent_sequence=? AND request_id=?`, intent.Parent.Principal.SwarmID, intent.Parent.Sequence, intent.RequestID).Scan(&result.ChildID)
	if err == nil {
		membership, err := s.GetEphemeralChildIntentForTask(ctx, result.ChildID)
		if err != nil {
			return EphemeralChildHistory{}, err
		}
		if membership.ID != intent.ID {
			return EphemeralChildHistory{}, ErrSwarmChildConflict
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return EphemeralChildHistory{}, err
	}
	executorFailure, err := s.GetEphemeralChildExecutorFailure(ctx, intent.ID)
	if err == nil {
		if executorFailure.ChildID != result.ChildID || executorFailure.RequestID != intent.RequestID || result.Failure != nil {
			return EphemeralChildHistory{}, ErrSwarmChildConflict
		}
		result.ExecutorFailure = &executorFailure
	} else if !errors.Is(err, ErrNotFound) {
		return EphemeralChildHistory{}, err
	}
	seal, err := s.GetEphemeralChildResultSeal(ctx, intent.ID)
	if err == nil {
		if seal.ChildID != result.ChildID || result.ExecutorFailure != nil {
			return EphemeralChildHistory{}, ErrSwarmChildConflict
		}
		result.ResultSeal = &seal
	} else if !errors.Is(err, ErrNotFound) {
		return EphemeralChildHistory{}, err
	}
	return result, nil
}
