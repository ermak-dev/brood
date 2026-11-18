package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

// EphemeralChildResultSeal retains exact external report and computed join
// evidence. It is internal history, not visibility or fresh lifecycle permission.
// The retained hashes bind original bytes, not a delivery receipt or source proof.
type EphemeralChildResultSeal struct {
	IntentID, ChildID, Outcome, ReportHash, ParentOutcome, ParentReportHash, CreatedAt string
	ChildRevision                                                                      int64
}

func ephemeralResultHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// GetEphemeralChildResultSeal reads immutable history without granting authority.
func (s *Store) GetEphemeralChildResultSeal(ctx context.Context, intentID string) (EphemeralChildResultSeal, error) {
	var out EphemeralChildResultSeal
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT intent_id,child_id,child_revision,outcome,report_hash,parent_outcome,parent_report_hash,created_at FROM ephemeral_child_result_seals WHERE intent_id=?`, intentID).Scan(&out.IntentID, &out.ChildID, &out.ChildRevision, &out.Outcome, &out.ReportHash, &out.ParentOutcome, &out.ParentReportHash, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EphemeralChildResultSeal{}, ErrNotFound
	}
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	return out, nil
}

func (s *Store) currentEphemeralResult(ctx context.Context, id string, authorize SwarmChildAuthorize) (EphemeralChildResultSeal, error) {
	if authorize == nil {
		return EphemeralChildResultSeal{}, ErrSwarmChildDenied
	}
	intent, err := s.GetEphemeralChildIntent(ctx, id)
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	allocation, err := s.GetEphemeralChildAllocation(ctx, id)
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	join, err := s.loadChildJoin(ctx, intent.Parent)
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	var child SwarmChildTask
	for _, candidate := range join.Children {
		if candidate.RequestID == intent.RequestID {
			child = candidate
			break
		}
	}
	membership, err := s.GetEphemeralChildIntentForTask(ctx, child.ID)
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	if membership.ID != intent.ID || child.Executor.ActorUID != allocation.ActorUID || child.Executor.Incarnation != allocation.ActorIncarnation || child.State != "completed" || strings.TrimSpace(child.Report) == "" || join.State != "open" || join.Interrupted || join.Evaluation.Open != 0 {
		return EphemeralChildResultSeal{}, ErrSwarmChildConflict
	}
	if err := validStepReport(child.Report); err != nil {
		return EphemeralChildResultSeal{}, err
	}
	if err := authorize(ctx, SwarmChildAuthorization{Action: "seal_ephemeral_result", Caller: intent.Parent.Principal, Parent: intent.Parent, Executor: child.Executor, RequestID: intent.RequestID, Payload: intent.Payload}); err != nil {
		return EphemeralChildResultSeal{}, err
	}
	if err := s.childIdentity(ctx, intent.Parent.Principal); err != nil {
		return EphemeralChildResultSeal{}, err
	}
	actor, err := s.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	if actor.Incarnation != allocation.ActorIncarnation || actor.Class != ActorClassEphemeral || actor.Role != "worker" || actor.ParentUID != intent.Parent.Principal.ActorUID || actor.SwarmID != intent.Parent.Principal.SwarmID || actor.AtespaceUID != intent.Parent.Principal.AtespaceUID || actor.PoolUID != intent.PoolUID || actor.Template != intent.Template || actor.SourceFingerprint != intent.SourceFingerprint || actor.Origin != intent.Origin {
		return EphemeralChildResultSeal{}, ErrSwarmChildConflict
	}
	if _, err := s.CurrentActorOperation(ctx, actor.UID); err == nil {
		return EphemeralChildResultSeal{}, ErrSwarmChildConflict
	} else if !errors.Is(err, ErrNotFound) {
		return EphemeralChildResultSeal{}, err
	}
	var intervened bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE (actor_uid=? AND incarnation=?) OR (actor_uid=? AND incarnation=?))`, actor.UID, actor.Incarnation, intent.Parent.Principal.ActorUID, intent.Parent.Principal.Incarnation).Scan(&intervened); err != nil {
		return EphemeralChildResultSeal{}, err
	}
	if intervened {
		return EphemeralChildResultSeal{}, ErrSwarmChildConflict
	}
	visit, err := s.GetSwarmStep(ctx, actor.SwarmID, intent.Parent.Sequence)
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	_, _, commit, err := s.stepPass(ctx, actor.SwarmID, intent.Parent.Sequence)
	if err != nil {
		return EphemeralChildResultSeal{}, err
	}
	parentOutcome := "fail"
	if join.Evaluation.State == "ready_success" {
		parentOutcome = "success"
	} else if join.Evaluation.State != "ready_fail" {
		return EphemeralChildResultSeal{}, ErrSwarmChildConflict
	}
	parentReport := ChildJoinReport(join)
	if visit.Pass != intent.Parent.Pass || visit.ActorUID != intent.Parent.Principal.ActorUID || visit.ActorIncarnation != intent.Parent.Principal.Incarnation || visit.RequestID != intent.Parent.RequestID || visit.AttestedCommit != intent.SourceCommit || commit != intent.SourceCommit || visit.State != "completed" || visit.Outcome == nil || *visit.Outcome != parentOutcome || visit.Report != parentReport {
		return EphemeralChildResultSeal{}, ErrSwarmChildConflict
	}
	return EphemeralChildResultSeal{IntentID: id, ChildID: child.ID, ChildRevision: child.Revision, Outcome: child.Outcome, ReportHash: ephemeralResultHash(child.Report), ParentOutcome: parentOutcome, ParentReportHash: ephemeralResultHash(parentReport)}, nil
}

// SealEphemeralChildResult durably binds a completed child's nonempty report and
// the server-computed, applied parent join. A new FULL write also retains earlier
// normal-WAL report commits before returning. Every retry reauthorizes the exact
// current scope; all nesting is refused. No provider effect or capacity release
// occurs here. Stop/delete still require separate current ownership and intent.
func (s *Store) SealEphemeralChildResult(ctx context.Context, intentID string, authorize SwarmChildAuthorize) error {
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		current, err := s.currentEphemeralResult(ctx, intentID, authorize)
		if err != nil {
			return err
		}
		old, err := s.GetEphemeralChildResultSeal(ctx, intentID)
		if err == nil {
			current.CreatedAt = old.CreatedAt
			if current != old {
				return ErrSwarmChildConflict
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		result, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO ephemeral_child_result_seals(intent_id,child_id,child_revision,outcome,report_hash,parent_outcome,parent_report_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`, current.IntentID, current.ChildID, current.ChildRevision, current.Outcome, current.ReportHash, current.ParentOutcome, current.ParentReportHash, nowRFC3339())
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrSwarmChildConflict
		}
		return nil
	})
}

// CheckEphemeralChildResultSeal rechecks retained evidence and current local
// authority. It is read-only and never authorizes a fresh physical mutation.
func (s *Store) CheckEphemeralChildResultSeal(ctx context.Context, intentID string, authorize SwarmChildAuthorize) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.currentEphemeralResult(ctx, intentID, authorize)
		if err != nil {
			return err
		}
		old, err := s.GetEphemeralChildResultSeal(ctx, intentID)
		if err != nil {
			return err
		}
		current.CreatedAt = old.CreatedAt
		if current != old {
			return ErrSwarmChildConflict
		}
		return nil
	})
}
