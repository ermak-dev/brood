package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/pipeline"
)

// PipelineNativeReservation identifies an immutable native actor creation request.
// Replay never authorizes a second creation, regardless of State.
// The caller must supply an externally verified immutable template UID and digest;
// the store checks their format and consistency, not their authenticity.
type PipelineNativeReservation struct {
	SwarmID, RunID, NodeID, RequestID         string
	Sequence                                  int64
	TemplateName, TemplateUID, TemplateDigest string
	ActorName, State                          string
	Replay                                    bool
}

func validNativeReservation(r PipelineNativeReservation) bool {
	if !validPipelineField(r.SwarmID) || !validPipelineField(r.RunID) || !validPipelineField(r.NodeID) || !validPipelineField(r.RequestID) || r.Sequence < 1 || !validPipelineField(r.TemplateName) || !validPipelineField(r.TemplateUID) {
		return false
	}
	if len(r.TemplateDigest) != 71 || !strings.HasPrefix(r.TemplateDigest, "sha256:") {
		return false
	}
	b, err := hex.DecodeString(r.TemplateDigest[7:])
	return err == nil && len(b) == 32 && r.TemplateDigest == strings.ToLower(r.TemplateDigest)
}

func nativeActorName(r PipelineNativeReservation) string {
	h := sha256.New()
	for _, v := range []string{r.SwarmID, r.RunID, fmt.Sprint(r.Sequence)} {
		fmt.Fprintf(h, "%d:%s", len(v), v)
	}
	return "pipeline-" + hex.EncodeToString(h.Sum(nil)[:20])
}

// ReservePipelineNativeActor persists the creation intent before any external effect.
// A false Replay is the sole authorization to attempt creation; on any error do not send.
func (s *Store) ReservePipelineNativeActor(ctx context.Context, r PipelineNativeReservation) (PipelineNativeReservation, error) {
	r.Replay = false
	r.State = ""
	r.ActorName = ""
	if !validNativeReservation(r) {
		return r, errors.New("invalid native pipeline reservation")
	}
	r.ActorName = nativeActorName(r)
	err := s.WithTx(ctx, func(ctx context.Context) error {
		run, err := s.GetPipelineRun(ctx, r.SwarmID, r.RunID)
		if err != nil {
			return err
		}
		if run.Source != nil {
			return ErrPipelineSourceUnverified
		}
		var seq int64
		var node, name, tn, tu, td, state string
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,COALESCE(native_actor_name,''),COALESCE(native_template_name,''),COALESCE(native_template_uid,''),COALESCE(native_template_digest,''),state FROM pipeline_steps WHERE run_id=? AND request_id=?`, r.RunID, r.RequestID).Scan(&seq, &node, &name, &tn, &tu, &td, &state)
		if err == nil {
			if seq != r.Sequence || node != r.NodeID || name != r.ActorName || tn != r.TemplateName || tu != r.TemplateUID || td != r.TemplateDigest {
				return ErrPipelineVisitConflict
			}
			r.State = state
			r.Replay = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if run.Status != "running" && run.Status != "waiting_human" {
			return ErrPipelineVisitConflict
		}
		d, err := pipeline.Parse(string(run.Graph), "pipeline")
		if err != nil {
			return err
		}
		n, ok := d.Nodes[r.NodeID]
		if !ok || n.Type != "actor" || n.Template != r.TemplateName {
			return ErrPipelineVisitConflict
		}
		var count, latest int64
		if err = s.conn(ctx).QueryRowContext(ctx, `SELECT count(*),COALESCE(MAX(sequence),0) FROM pipeline_steps WHERE run_id=?`, r.RunID).Scan(&count, &latest); err != nil {
			return err
		}
		if count == 0 {
			if r.Sequence != 1 || d.Start != r.NodeID {
				return ErrPipelineVisitConflict
			}
			_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_steps(run_id,sequence,node_id,state,request_id,native_actor_name,native_template_name,native_template_uid,native_template_digest) VALUES(?,?,?,'inflight',?,?,?,?,?)`, r.RunID, r.Sequence, r.NodeID, r.RequestID, r.ActorName, r.TemplateName, r.TemplateUID, r.TemplateDigest)
		} else {
			if latest != r.Sequence {
				return ErrPipelineVisitConflict
			}
			var result sql.Result
			result, err = s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state='inflight',request_id=?,native_actor_name=?,native_template_name=?,native_template_uid=?,native_template_digest=? WHERE run_id=? AND sequence=? AND node_id=? AND state='pending'`, r.RequestID, r.ActorName, r.TemplateName, r.TemplateUID, r.TemplateDigest, r.RunID, r.Sequence, r.NodeID)
			if err == nil {
				var changed int64
				changed, err = result.RowsAffected()
				if err == nil && changed != 1 {
					return ErrPipelineVisitConflict
				}
			}
		}
		if err == nil {
			r.State = "inflight"
		}
		return err
	})
	return r, err
}

// BindPipelineNativeActor records an identity only after the caller verifies creation.
// An unconfirmed reservation cannot be bound without an explicit reconciliation protocol.
func (s *Store) BindPipelineNativeActor(ctx context.Context, r PipelineNativeReservation, uid string, incarnation int64) error {
	if !validNativeReservation(r) || !validPipelineField(uid) || incarnation < 1 {
		return errors.New("invalid native pipeline binding")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		run, err := s.GetPipelineRun(ctx, r.SwarmID, r.RunID)
		if err != nil {
			return err
		}
		if run.Source != nil {
			return ErrPipelineSourceUnverified
		}
		name := nativeActorName(r)
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET actor_uid=?,actor_incarnation=? WHERE run_id=? AND sequence=? AND node_id=? AND request_id=? AND native_actor_name=? AND native_template_name=? AND native_template_uid=? AND native_template_digest=? AND state='inflight' AND actor_uid IS NULL AND actor_incarnation IS NULL AND sequence=(SELECT MAX(sequence) FROM pipeline_steps WHERE run_id=?)`, uid, incarnation, r.RunID, r.Sequence, r.NodeID, r.RequestID, name, r.TemplateName, r.TemplateUID, r.TemplateDigest, r.RunID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		var count int
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT count(*) FROM pipeline_steps WHERE run_id=? AND sequence=? AND node_id=? AND request_id=? AND native_actor_name=? AND native_template_name=? AND native_template_uid=? AND native_template_digest=? AND actor_uid=? AND actor_incarnation=? AND state='inflight' AND sequence=(SELECT MAX(sequence) FROM pipeline_steps WHERE run_id=?)`, r.RunID, r.Sequence, r.NodeID, r.RequestID, name, r.TemplateName, r.TemplateUID, r.TemplateDigest, uid, incarnation, r.RunID).Scan(&count)
		if err != nil {
			return err
		}
		if count == 1 {
			return nil
		}
		return ErrPipelineVisitConflict
	})
}

// MarkPipelineNativeActorUncertain records an ambiguous creation outcome without resending.
func (s *Store) MarkPipelineNativeActorUncertain(ctx context.Context, r PipelineNativeReservation) error {
	if !validNativeReservation(r) {
		return errors.New("invalid native pipeline reservation")
	}
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state='unconfirmed' WHERE run_id=? AND sequence=? AND node_id=? AND request_id=? AND native_actor_name=? AND native_template_name=? AND native_template_uid=? AND native_template_digest=? AND state IN ('inflight','unconfirmed') AND actor_uid IS NULL AND actor_incarnation IS NULL AND run_id IN (SELECT id FROM pipeline_runs WHERE swarm_id=?)`, r.RunID, r.Sequence, r.NodeID, r.RequestID, nativeActorName(r), r.TemplateName, r.TemplateUID, r.TemplateDigest, r.SwarmID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrPipelineVisitConflict
	}
	return nil
}
