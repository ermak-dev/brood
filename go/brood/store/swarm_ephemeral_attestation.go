package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// EphemeralChildAttestation is internal retained history, never a public DTO.
// Only a successful fresh reservation with Replay=false permits one future RPC.
// No signature, nonce, challenge, payload or root workspace lease is exposed.
type EphemeralChildAttestation struct {
	IntentID, AttemptID, ActorUID                                               string
	ActorIncarnation                                                            int64
	Origin, Commit, State, AttestedOrigin, AttestedCommit, CreatedAt, UpdatedAt string
	Replay                                                                      bool
}

type ephemeralAttestationRecord struct {
	EphemeralChildAttestation
	lease ActorOperation
}

// Callback читает только локальные права через transaction ctx, без эффектов и сети.
func (s *Store) currentEphemeralAttestationScope(ctx context.Context, intentID string, authorize SwarmChildAuthorize) (EphemeralChildIntent, EphemeralChildAllocation, error) {
	return s.currentEphemeralChildScope(ctx, intentID, "attest_ephemeral", false, authorize)
}

func (s *Store) currentEphemeralChildScope(ctx context.Context, intentID, action string, registered bool, authorize SwarmChildAuthorize) (EphemeralChildIntent, EphemeralChildAllocation, error) {
	return s.currentEphemeralChildBindingScope(ctx, intentID, action, registered, true, authorize)
}

// Только create допускает ещё не ready executor; attestation/delivery сохраняют childIdentity.
func (s *Store) currentEphemeralChildBindingScope(ctx context.Context, intentID, action string, registered, ready bool, authorize SwarmChildAuthorize) (EphemeralChildIntent, EphemeralChildAllocation, error) {
	intent, err := s.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return intent, EphemeralChildAllocation{}, err
	}
	allocation, err := s.GetEphemeralChildAllocation(ctx, intentID)
	if err != nil {
		return intent, allocation, err
	}
	executor := intent.Parent.Principal
	executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
	if err := s.authorizeChild(ctx, SwarmChildAuthorization{Action: action, Caller: intent.Parent.Principal, Parent: intent.Parent, Executor: executor, RequestID: intent.RequestID, Payload: intent.Payload}, authorize); err != nil {
		return intent, allocation, err
	}
	if err := s.checkEphemeralChildNotFailed(ctx, intentID); err != nil {
		return intent, allocation, err
	}
	if action != "fail_ephemeral_executor" {
		if _, err := s.GetEphemeralChildExecutorFailure(ctx, intentID); err == nil {
			return intent, allocation, ErrSwarmChildConflict
		} else if !errors.Is(err, ErrNotFound) {
			return intent, allocation, err
		}
	}
	if !registered {
		join, err := s.loadChildJoin(ctx, intent.Parent)
		if err != nil {
			return intent, allocation, err
		}
		for _, child := range join.Children {
			if child.RequestID == intent.RequestID {
				return intent, allocation, ErrSwarmChildConflict
			}
		}
	}
	return intent, allocation, s.checkEphemeralChildBinding(ctx, intent, allocation, ready)
}

func (s *Store) checkEphemeralChildScope(ctx context.Context, intent EphemeralChildIntent, allocation EphemeralChildAllocation) error {
	return s.checkEphemeralChildBinding(ctx, intent, allocation, true)
}

func (s *Store) checkEphemeralChildBinding(ctx context.Context, intent EphemeralChildIntent, allocation EphemeralChildAllocation, ready bool) error {
	executor := intent.Parent.Principal
	executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
	if err := s.currentChildParent(ctx, intent.Parent, false); err != nil {
		return err
	}
	join, err := s.loadChildJoin(ctx, intent.Parent)
	if err != nil {
		return err
	}
	if join.State != "open" || join.Interrupted {
		return ErrSwarmChildConflict
	}
	if ready {
		if err := s.childIdentity(ctx, executor); err != nil {
			return err
		}
	}
	actor, err := s.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return err
	}
	if actor.Incarnation != allocation.ActorIncarnation || actor.SwarmID != executor.SwarmID || actor.AtespaceUID != executor.AtespaceUID || actor.Class != ActorClassEphemeral || actor.Role != "worker" || actor.ParentUID != intent.Parent.Principal.ActorUID || actor.PoolUID != intent.PoolUID || actor.Origin != intent.Origin || actor.Template != intent.Template || actor.SourceFingerprint != intent.SourceFingerprint {
		return ErrSwarmChildConflict
	}
	swarm, err := s.GetSwarm(ctx, intent.Parent.Principal.SwarmID)
	if err != nil {
		return err
	}
	_, _, commit, err := s.stepPass(ctx, swarm.ID, intent.Parent.Sequence)
	if err != nil {
		return err
	}
	if swarm.Source == nil || swarm.Source.Origin != intent.Origin || commit == "" || commit != intent.SourceCommit {
		return ErrPipelineSourceUnverified
	}
	return nil
}

// ReserveEphemeralChildAttestation durably reserves one attempt and workspace
// lease before any future RPC. Exact retries reauthorize but return history only;
// no timeout, alternate attempt ID or recovery may grant another effect permission.
// One intent supports one attempt, including after success. No child is registered.
func (s *Store) ReserveEphemeralChildAttestation(ctx context.Context, intentID, attemptID string, authorize SwarmChildAuthorize) (EphemeralChildAttestation, error) {
	if !validPipelineField(intentID) || !validChildRequestID(attemptID) {
		return EphemeralChildAttestation{}, ErrSwarmChildConflict
	}
	var out EphemeralChildAttestation
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		intent, allocation, err := s.currentEphemeralAttestationScope(ctx, intentID, authorize)
		if err != nil {
			return err
		}
		old, err := s.ephemeralChildAttestation(ctx, intentID)
		if err == nil {
			if old.AttemptID != attemptID {
				return ErrSwarmChildConflict
			}
			out = old.EphemeralChildAttestation
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		lease, err := s.AcquireActorOperation(ctx, allocation.ActorUID, allocation.ActorIncarnation, "ephemeral-attest:"+intentID+":"+attemptID, "ephemeral:"+intentID, "workspace_attest")
		if err != nil {
			return err
		}
		now := nowRFC3339()
		out = EphemeralChildAttestation{IntentID: intentID, AttemptID: attemptID, ActorUID: allocation.ActorUID, ActorIncarnation: allocation.ActorIncarnation, Origin: intent.Origin, Commit: intent.SourceCommit, State: "pending", CreatedAt: now, UpdatedAt: now}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO ephemeral_child_attestations(intent_id,attempt_id,actor_uid,actor_incarnation,origin,source_commit,lease,state,attested_origin,attested_commit,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending','','',?,?)`, intentID, attemptID, out.ActorUID, out.ActorIncarnation, out.Origin, out.Commit, encodeManaged(lease), now, now)
		return err
	})
	if err != nil {
		return EphemeralChildAttestation{}, err
	}
	return out, nil
}

func (s *Store) ephemeralChildAttestation(ctx context.Context, intentID string) (ephemeralAttestationRecord, error) {
	var out ephemeralAttestationRecord
	var lease string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT intent_id,attempt_id,actor_uid,actor_incarnation,origin,source_commit,lease,state,attested_origin,attested_commit,created_at,updated_at FROM ephemeral_child_attestations WHERE intent_id=?`, intentID).Scan(&out.IntentID, &out.AttemptID, &out.ActorUID, &out.ActorIncarnation, &out.Origin, &out.Commit, &lease, &out.State, &out.AttestedOrigin, &out.AttestedCommit, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(lease), &out.lease); err != nil {
		return ephemeralAttestationRecord{}, err
	}
	out.Replay = true
	return out, nil
}

// GetEphemeralChildAttestation reads internal history, never effect permission or
// caller authority. Adapters must enforce visibility. The workspace lease stays internal.
func (s *Store) GetEphemeralChildAttestation(ctx context.Context, intentID string) (EphemeralChildAttestation, error) {
	out, err := s.ephemeralChildAttestation(ctx, intentID)
	if err != nil {
		return EphemeralChildAttestation{}, err
	}
	return out.EphemeralChildAttestation, nil
}

func (s *Store) currentEphemeralAttestation(ctx context.Context, intentID, attemptID string, authorize SwarmChildAuthorize) (ephemeralAttestationRecord, error) {
	intent, allocation, err := s.currentEphemeralAttestationScope(ctx, intentID, authorize)
	if err != nil {
		return ephemeralAttestationRecord{}, err
	}
	out, err := s.ephemeralChildAttestation(ctx, intentID)
	if err != nil {
		return out, err
	}
	if out.AttemptID != attemptID || out.ActorUID != allocation.ActorUID || out.ActorIncarnation != allocation.ActorIncarnation || out.Origin != intent.Origin || out.Commit != intent.SourceCommit {
		return out, ErrSwarmChildConflict
	}
	return out, nil
}

// Допустима только смена inflight на unconfirmed; вмешательство владельца
// всегда блокирует принятие доказательства от прежней операции.
func (s *Store) checkEphemeralAttestationLease(ctx context.Context, out ephemeralAttestationRecord) error {
	current, err := s.CurrentActorOperation(ctx, out.ActorUID)
	if err != nil {
		return err
	}
	if current.InterruptedBy != "" {
		return ErrSwarmChildConflict
	}
	current.State = out.lease.State
	if current != out.lease {
		return ErrSwarmChildConflict
	}
	return nil
}

// UnconfirmEphemeralChildAttestation retains an unknown outcome and its exact
// workspace hold in FULL sync. It never releases, expires or rearms an attempt.
func (s *Store) UnconfirmEphemeralChildAttestation(ctx context.Context, intentID, attemptID string, authorize SwarmChildAuthorize) error {
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		out, err := s.currentEphemeralAttestation(ctx, intentID, attemptID, authorize)
		if err != nil {
			return err
		}
		if out.State == "attested" {
			return ErrSwarmChildConflict
		}
		if err := s.checkEphemeralAttestationLease(ctx, out); err != nil {
			return err
		}
		if err := s.FinishActorOperation(ctx, out.lease, false); err != nil {
			return err
		}
		if out.State == "unconfirmed" {
			return nil
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE ephemeral_child_attestations SET state='unconfirmed',updated_at=? WHERE intent_id=?`, nowRFC3339(), intentID)
		return err
	})
}

// RecordEphemeralChildAttestation accepts already server-verified proof, at the
// same trust boundary as RecordActorStepAttestationDelivery. Store does not verify
// crypto or persist its challenge/signature. Current parent, allocation, source
// and exact lease are rechecked; proof and lease release commit together in FULL
// sync. This grants no instruction delivery or child registration permission.
func (s *Store) RecordEphemeralChildAttestation(ctx context.Context, intentID, attemptID, origin, commit string, authorize SwarmChildAuthorize) error {
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		out, err := s.currentEphemeralAttestation(ctx, intentID, attemptID, authorize)
		if err != nil {
			return err
		}
		if origin != out.Origin || commit == "" || commit != out.Commit {
			return ErrPipelineSourceUnverified
		}
		if out.State == "attested" {
			return nil
		}
		if err := s.checkEphemeralAttestationLease(ctx, out); err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE ephemeral_child_attestations SET state='attested',attested_origin=?,attested_commit=?,updated_at=? WHERE intent_id=?`, origin, commit, nowRFC3339(), intentID)
		if err != nil {
			return err
		}
		return s.FinishActorOperation(ctx, out.lease, true)
	})
}

// RecoverEphemeralChildAttestations marks pending attempts and their held leases
// unconfirmed, without releasing or retrying anything. Call only during exclusive
// startup after the previous server stopped. It needs no live-parent authority:
// historical uncertainty survives parent stop, incarnation changes and intervention.
func (s *Store) RecoverEphemeralChildAttestations(ctx context.Context) error {
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		rows, err := s.conn(ctx).QueryContext(ctx, `SELECT intent_id FROM ephemeral_child_attestations WHERE state='pending' ORDER BY intent_id`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			out, err := s.ephemeralChildAttestation(ctx, id)
			if err != nil {
				return err
			}
			current, err := s.CurrentActorOperation(ctx, out.ActorUID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			// Явное освобождение и смена владельца не позволяют recovery
			// восстановить старый hold или изменить операцию-преемника.
			if err == nil {
				current.State, current.InterruptedBy = out.lease.State, out.lease.InterruptedBy
				if current == out.lease {
					if err := s.FinishActorOperation(ctx, out.lease, false); err != nil {
						return err
					}
				}
			}
			if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE ephemeral_child_attestations SET state='unconfirmed',updated_at=? WHERE intent_id=?`, nowRFC3339(), id); err != nil {
				return err
			}
		}
		return nil
	})
}
