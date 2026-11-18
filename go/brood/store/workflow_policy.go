package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	"brood.dev/go/brood/workflowpolicy"
)

// MaxWorkflowPolicyBytes bounds the JSON-encoded rules of one retained version.
const MaxWorkflowPolicyBytes = 131072

// ErrWorkflowPolicyConflict means the expected project policy revision is stale.
var ErrWorkflowPolicyConflict = errors.New("workflow policy revision changed")

// ErrWorkflowPolicyDenied means current administrator or explicit project owner authority is absent.
var ErrWorkflowPolicyDenied = errors.New("workflow policy authorization changed or access denied")

// WorkflowPolicyVersion is immutable configuration, not an active scheduling gate.
// Revision zero is the implicit empty configuration of an existing unconfigured project.
type WorkflowPolicyVersion struct {
	ProjectID string
	Revision  int64
	Rules     []workflowpolicy.Rule
	CreatedBy string
	CreatedAt string
}

// ProjectWorkflowPolicy reads current configuration without granting execution authority.
// The caller enforces project visibility; current configuration requires a live project.
func (s *Store) ProjectWorkflowPolicy(ctx context.Context, projectID string) (WorkflowPolicyVersion, error) {
	var revision int64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT COALESCE(h.revision,0) FROM projects p LEFT JOIN project_workflow_policy_heads h ON h.project_id=p.id WHERE p.id=?`, projectID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowPolicyVersion{}, ErrNotFound
	}
	if err != nil {
		return WorkflowPolicyVersion{}, err
	}
	if revision == 0 {
		return WorkflowPolicyVersion{ProjectID: projectID, Rules: []workflowpolicy.Rule{}}, nil
	}
	return s.ProjectWorkflowPolicyVersion(ctx, projectID, revision)
}

// ProjectWorkflowPolicyVersion reads retained history, including a deleted project's versions.
// Callers must independently authorize historical material access.
func (s *Store) ProjectWorkflowPolicyVersion(ctx context.Context, projectID string, revision int64) (WorkflowPolicyVersion, error) {
	result := WorkflowPolicyVersion{ProjectID: projectID, Revision: revision}
	var raw string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT rules,created_by,created_at FROM project_workflow_policy_versions WHERE project_id=? AND revision=?`, projectID, revision).Scan(&raw, &result.CreatedBy, &result.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowPolicyVersion{}, ErrNotFound
	}
	if err != nil {
		return WorkflowPolicyVersion{}, err
	}
	if err = json.Unmarshal([]byte(raw), &result.Rules); err != nil {
		return WorkflowPolicyVersion{}, errors.New("stored workflow policy is invalid")
	}
	if _, err = workflowpolicy.New(result.Rules); err != nil {
		return WorkflowPolicyVersion{}, errors.New("stored workflow policy is invalid")
	}
	return result, nil
}

// SetProjectWorkflowPolicy appends a version and CAS-updates its current pointer atomically.
// authorize must recheck the request session using the supplied transaction context.
// Current installation administrator or explicit active project owner authority is required.
func (s *Store) SetProjectWorkflowPolicy(ctx context.Context, actorID, projectID string, expected int64, rules []workflowpolicy.Rule, authorize func(context.Context) bool) (WorkflowPolicyVersion, error) {
	if expected < 0 || expected == math.MaxInt64 {
		return WorkflowPolicyVersion{}, ErrWorkflowPolicyConflict
	}
	if _, err := workflowpolicy.New(rules); err != nil {
		return WorkflowPolicyVersion{}, err
	}
	if rules == nil {
		rules = []workflowpolicy.Rule{}
	}
	encoded, err := json.Marshal(rules)
	if err != nil || len(encoded) > MaxWorkflowPolicyBytes {
		return WorkflowPolicyVersion{}, errors.New("workflow policy exceeds storage limit")
	}
	var result WorkflowPolicyVersion
	err = s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrWorkflowPolicyDenied
		}
		allowed, err := s.CanManageProjectWorkflowPolicy(ctx, actorID, projectID)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrWorkflowPolicyDenied
		}
		current, err := s.ProjectWorkflowPolicy(ctx, projectID)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrWorkflowPolicyConflict
		}
		revision := expected + 1
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO project_workflow_policy_versions(project_id,revision,rules,created_by,created_at) VALUES(?,?,?,?,?)`, projectID, revision, string(encoded), actorID, nowRFC3339())
		if err != nil {
			return err
		}
		if expected == 0 {
			_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO project_workflow_policy_heads(project_id,revision) VALUES(?,?)`, projectID, revision)
		} else {
			var update sql.Result
			update, err = s.conn(ctx).ExecContext(ctx, `UPDATE project_workflow_policy_heads SET revision=? WHERE project_id=? AND revision=?`, revision, projectID, expected)
			if err == nil {
				if n, _ := update.RowsAffected(); n != 1 {
					return ErrWorkflowPolicyConflict
				}
			}
		}
		if err != nil {
			return err
		}
		result, err = s.ProjectWorkflowPolicyVersion(ctx, projectID, revision)
		return err
	})
	if err != nil {
		return WorkflowPolicyVersion{}, err
	}
	return result, nil
}
