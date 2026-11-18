package store

import (
	"context"
	"database/sql"
	"errors"

	"brood.dev/go/brood/workflowpolicy"
)

// SwarmPassPolicy identifies the immutable configuration captured at pass admission.
// LegacyEmpty distinguishes pre-snapshot passes from newly admitted empty policies.
// Capturing policy does not itself enforce automatic transition requirements.
type SwarmPassPolicy struct {
	SwarmID     string
	Pass        int64
	ProjectID   string
	Revision    int64
	LegacyEmpty bool
	Rules       []workflowpolicy.Rule
}

// SwarmStepWorkflowPolicy reads the policy of an existing step's exact pass.
// It never consults the mutable project default. Missing snapshots fail explicitly.
// Callers must authorize access to the step's swarm before reading its material.
func (s *Store) SwarmStepWorkflowPolicy(ctx context.Context, swarmID string, sequence int64) (SwarmPassPolicy, error) {
	result := SwarmPassPolicy{SwarmID: swarmID, Rules: []workflowpolicy.Rule{}}
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT p.pass_sequence,p.project_id,COALESCE(p.policy_revision,0),p.legacy_empty
 FROM swarm_steps st JOIN swarm_pass_policies p ON p.swarm_id=st.swarm_id AND p.pass_sequence=(
  SELECT MAX(first.sequence) FROM swarm_steps first WHERE first.swarm_id=st.swarm_id AND first.sequence<=st.sequence AND first.goal IS NOT NULL
 ) WHERE st.swarm_id=? AND st.sequence=?`, swarmID, sequence).Scan(&result.Pass, &result.ProjectID, &result.Revision, &result.LegacyEmpty)
	if errors.Is(err, sql.ErrNoRows) {
		return SwarmPassPolicy{}, ErrNotFound
	}
	if err != nil {
		return SwarmPassPolicy{}, err
	}
	if result.Revision > 0 {
		version, err := s.ProjectWorkflowPolicyVersion(ctx, result.ProjectID, result.Revision)
		if err != nil {
			return SwarmPassPolicy{}, err
		}
		result.Rules = version.Rules
	}
	return result, nil
}

func (s *Store) pinPassWorkflowPolicy(ctx context.Context, swarmID string, sequence int64) error {
	var projectID string
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT project_id FROM swarms WHERE id=?`, swarmID).Scan(&projectID); err != nil {
		return err
	}
	policy, err := s.ProjectWorkflowPolicy(ctx, projectID)
	if err != nil {
		return err
	}
	_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_pass_policies(swarm_id,pass_sequence,project_id,policy_revision,legacy_empty) VALUES(?,?,?,NULLIF(?,0),0)`, swarmID, sequence, projectID, policy.Revision)
	return err
}
