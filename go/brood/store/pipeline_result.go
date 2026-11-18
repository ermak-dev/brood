package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"brood.dev/go/brood/proxmox/attest"
)

// WorkspaceResultOperation reserves the actor during root-supervised result observation.
const WorkspaceResultOperation = "workspace_result"

// PipelineResultObservation pins one non-repeatable observation and its workspace lease.
// An inflight record may already have stopped the actor; uncertainty never authorizes retry.
type PipelineResultObservation struct {
	ID       string
	Intent   PipelineCompletionIntent
	Request  attest.ResultRequest
	Lease    ActorOperation
	State    string
	Detail   string
	Revision int64
}

// GetPipelineResultObservation reads retained state; callers enforce swarm visibility.
func (s *Store) GetPipelineResultObservation(ctx context.Context, swarmID string, sequence int64) (PipelineResultObservation, error) {
	var result PipelineResultObservation
	var request, lease string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT id,request,lease,state,detail,revision FROM pipeline_result_observations WHERE swarm_id=? AND sequence=?`, swarmID, sequence).Scan(&result.ID, &request, &lease, &result.State, &result.Detail, &result.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if json.Unmarshal([]byte(request), &result.Request) != nil || json.Unmarshal([]byte(lease), &result.Lease) != nil {
		return PipelineResultObservation{}, errors.New("invalid stored result observation")
	}
	result.Intent, err = s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
	return result, err
}

func (s *Store) checkResultIntent(ctx context.Context, intent PipelineCompletionIntent) error {
	if err := s.requireCompletionReceipt(ctx, intent); err != nil {
		return err
	}
	// Только уже сохранённый server-checked intent разрешает повторно сверить
	// computed child_join; model payload сам по себе такого контекста не получает.
	checked, err := s.checkActorCompletion(context.WithValue(ctx, childJoinCompletionKey{}, true), intent.ActorCompletion)
	if err != nil {
		return err
	}
	if checked.replay || checked.visit.Pass != intent.Pass || checked.swarm.Source == nil || checked.swarm.Source.Origin != intent.Origin {
		return ErrPipelineVisitConflict
	}
	return s.checkResultCoordination(ctx, intent)
}

// Общие no-effect ограждения для positive result и terminal review rejection.
// Потеря роли может завершить review отказом, но не снимает manual/child claims.
func (s *Store) checkResultCoordination(ctx context.Context, intent PipelineCompletionIntent) error {
	if err := s.guardOwnerIntervention(ctx, intent.ActorUID); err != nil {
		return err
	}
	var held bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM swarm_child_tasks WHERE actor_uid=? AND state IN ('pending','running')) OR
 EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.parent_uid=? AND `+swarmChildUnfinished+` AND (j.swarm_id!=? OR j.parent_sequence!=?))`, intent.ActorUID, intent.ActorUID, intent.SwarmID, intent.Sequence).Scan(&held)
	if err != nil {
		return err
	}
	if held {
		return newCoordinationConflict("other child obligations hold this result workspace")
	}
	claims, err := s.ActorCoordinationClaims(ctx, intent.ActorUID)
	if err != nil {
		return err
	}
	if len(claims) != 0 {
		return newCoordinationConflict("dispatch or landing coordination still holds the result workspace")
	}
	return nil
}

// ReservePipelineResultObservation pins a server-issued challenge and acquires its lease.
// It is service orchestration, not an actor-selected root RPC. No guest effect occurs here.
func (s *Store) ReservePipelineResultObservation(ctx context.Context, swarmID string, sequence int64, id, caller string, request attest.ResultRequest) (PipelineResultObservation, error) {
	if err := request.Validate(); err != nil {
		return PipelineResultObservation{}, err
	}
	var result PipelineResultObservation
	err := s.WithTx(ctx, func(ctx context.Context) error {
		intent, err := s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		want := attest.ResultRequest{Claim: attest.Claim{ActorUID: intent.ActorUID, Generation: intent.Incarnation, Swarm: intent.SwarmID, Step: intent.RequestID, Origin: intent.Origin, Commit: intent.BaseCommit, Challenge: request.Challenge}, Pass: intent.Pass, Sequence: intent.Sequence}
		if request != want {
			return ErrPipelineVisitConflict
		}
		if _, err := s.GetPipelineResultObservation(ctx, swarmID, sequence); err == nil {
			return ErrPipelineVisitConflict
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := s.checkResultIntent(ctx, intent); err != nil {
			return err
		}
		lease, err := s.acquireActorOperation(ctx, intent.ActorUID, intent.Incarnation, id, caller, WorkspaceResultOperation, func(ctx context.Context, a ManagedActor) error {
			if err := s.checkResultIntent(ctx, intent); err != nil {
				return err
			}
			if _, err := s.CurrentActorOperation(ctx, a.UID); err == nil {
				return newCoordinationConflict("result workspace operation is active or unconfirmed")
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
		raw, err := json.Marshal(request)
		if err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_result_observations(id,swarm_id,sequence,request,lease,state,detail,revision) VALUES(?,?,?,?,?,'inflight','',1)`, id, swarmID, sequence, string(raw), encodeManaged(lease))
		if err != nil {
			return err
		}
		result, err = s.GetPipelineResultObservation(ctx, swarmID, sequence)
		return err
	})
	if err != nil {
		return PipelineResultObservation{}, err
	}
	return result, nil
}

func sameResultObservation(a, b PipelineResultObservation) bool {
	return a.ID == b.ID && a.Intent == b.Intent && a.Request == b.Request && a.Lease == b.Lease
}

func (s *Store) checkResultObservation(ctx context.Context, observation PipelineResultObservation) error {
	current, err := s.GetPipelineResultObservation(ctx, observation.Intent.SwarmID, observation.Intent.Sequence)
	if err != nil {
		return err
	}
	if !sameResultObservation(current, observation) || current.State != "inflight" || current.Revision != observation.Revision {
		return ErrPipelineVisitConflict
	}
	if err := s.checkResultIntent(ctx, current.Intent); err != nil {
		return err
	}
	lease, err := s.CurrentActorOperation(ctx, current.Intent.ActorUID)
	if err != nil {
		return err
	}
	if lease != current.Lease {
		return newCoordinationConflict("result observation lease changed or was interrupted")
	}
	return nil
}

// CheckPipelineResultObservation rechecks local state at the final transport boundary.
// The server must also serialize channel opening with explicit owner intervention.
func (s *Store) CheckPipelineResultObservation(ctx context.Context, observation PipelineResultObservation) error {
	return s.WithTx(ctx, func(ctx context.Context) error { return s.checkResultObservation(ctx, observation) })
}

// GetPipelineResultEvidence returns immutable signed evidence, not permission to advance.
func (s *Store) GetPipelineResultEvidence(ctx context.Context, swarmID string, sequence int64) (attest.ResultAttestation, error) {
	var raw string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT e.attestation FROM pipeline_result_evidence e JOIN pipeline_result_observations o ON o.id=e.observation_id WHERE o.swarm_id=? AND o.sequence=?`, swarmID, sequence).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return attest.ResultAttestation{}, ErrNotFound
	}
	if err != nil {
		return attest.ResultAttestation{}, err
	}
	return attest.DecodeResultAttestation([]byte(raw))
}

// RecordPipelineResultEvidence verifies the exact signed request and stores its evidence.
// The server must consume the issued challenge first; key derives from the reserved
// identity. Neither key nor relay token is stored. No graph transition occurs here.
func (s *Store) RecordPipelineResultEvidence(ctx context.Context, observation PipelineResultObservation, key []byte, signed attest.ResultAttestation) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.GetPipelineResultObservation(ctx, observation.Intent.SwarmID, observation.Intent.Sequence)
		if err != nil {
			return err
		}
		if !sameResultObservation(current, observation) {
			return ErrPipelineVisitConflict
		}
		if err := attest.VerifyResult(key, current.Request, signed); err != nil {
			return err
		}
		raw, err := json.Marshal(signed)
		if err != nil || len(raw) > attest.MaxResultAttestationBytes {
			return errors.New("result evidence exceeds storage limit")
		}
		if current.State == "observed" {
			existing, err := s.GetPipelineResultEvidence(ctx, current.Intent.SwarmID, current.Intent.Sequence)
			if err != nil {
				return err
			}
			prior, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			if string(prior) != string(raw) {
				return ErrPipelineVisitConflict
			}
			return nil
		}
		if err := s.checkResultObservation(ctx, observation); err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_result_evidence(observation_id,attestation) VALUES(?,?)`, current.ID, string(raw))
		if err != nil {
			return err
		}
		if err := s.settleResultObservation(ctx, current, "observed", ""); err != nil {
			return err
		}
		return s.FinishActorOperation(ctx, current.Lease, true)
	})
}

func (s *Store) settleResultObservation(ctx context.Context, observation PipelineResultObservation, state, detail string) error {
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_result_observations SET state=?,detail=?,revision=revision+1 WHERE id=? AND state='inflight' AND revision=?`, state, stepDetail(detail), observation.ID, observation.Revision)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrPipelineVisitConflict
	}
	return nil
}

// SettlePipelineResultObservation records a known refusal or retains an uncertain effect.
// Unknown outcomes keep the exact workspace reservation; neither outcome schedules a retry.
func (s *Store) SettlePipelineResultObservation(ctx context.Context, observation PipelineResultObservation, known bool, detail string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.GetPipelineResultObservation(ctx, observation.Intent.SwarmID, observation.Intent.Sequence)
		if err != nil {
			return err
		}
		if !sameResultObservation(current, observation) || current.State != "inflight" || current.Revision != observation.Revision {
			return ErrPipelineVisitConflict
		}
		state := "unconfirmed"
		if known {
			state = "refused"
		}
		if err := s.settleResultObservation(ctx, current, state, detail); err != nil {
			return err
		}
		return s.FinishActorOperation(ctx, current.Lease, known)
	})
}
