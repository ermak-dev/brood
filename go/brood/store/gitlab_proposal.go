package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Новая таблица не меняет историческую схему 33 или её миграцию.
const gitlabProposalSchema = `CREATE TABLE gitlab_proposal_states (
 operation_id TEXT PRIMARY KEY NOT NULL REFERENCES gitlab_operations(id),
 state TEXT NOT NULL CHECK (state IN ('pending','rejected','stale')),
 revision INTEGER NOT NULL CHECK (revision > 0),
 human_id TEXT NOT NULL DEFAULT '',
 session_hash TEXT NOT NULL DEFAULT ''
);`
const gitlabProposalFence = `CREATE TRIGGER gitlab_proposal_states_fence BEFORE UPDATE ON gitlab_proposal_states WHEN
 NEW.operation_id IS NOT OLD.operation_id OR OLD.state != 'pending' OR NEW.state NOT IN ('rejected','stale') OR NEW.revision != OLD.revision+1
 BEGIN SELECT RAISE(ABORT, 'GitLab proposal decision is terminal'); END`
const gitlabProposalNoDelete = `CREATE TRIGGER gitlab_proposal_states_no_delete BEFORE DELETE ON gitlab_proposal_states
 BEGIN SELECT RAISE(ABORT, 'GitLab proposal history cannot be deleted'); END`
const gitlabProposalNoReplace = `CREATE TRIGGER gitlab_proposal_states_no_replace BEFORE INSERT ON gitlab_proposal_states WHEN EXISTS (SELECT 1 FROM gitlab_proposal_states WHERE operation_id=NEW.operation_id)
 BEGIN SELECT RAISE(ABORT, 'GitLab proposal history cannot be replaced'); END`

func gitlabProposalFullSchema() string {
	return gitlabProposalSchema + gitlabProposalFence + ";" + gitlabProposalNoDelete + ";" + gitlabProposalNoReplace
}
func verifyGitLabProposalSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, gitlabProposalSchema); err != nil {
		return err
	}
	for name, ddl := range map[string]string{"gitlab_proposal_states_fence": gitlabProposalFence, "gitlab_proposal_states_no_delete": gitlabProposalNoDelete, "gitlab_proposal_states_no_replace": gitlabProposalNoReplace} {
		if err := checkObject(ctx, tx, "trigger", name, ddl); err != nil {
			return err
		}
	}
	return nil
}

// ReserveGitLabProposalWithGrant pins exact authority and immutable observed evidence.
// The server must obtain complete authoritative evidence before calling this method.
func (s *Store) ReserveGitLabProposalWithGrant(ctx context.Context, p GitLabPrincipal, request, grant string, snapshot GitLabMergeSnapshot) (GitLabOperation, error) {
	if grant == "" || snapshot.Requester != p || snapshot.MRIID <= 0 || snapshot.PipelineID <= 0 || !gitlabSHA.MatchString(snapshot.SourceSHA) || !gitlabSHA.MatchString(snapshot.TargetSHA) || snapshot.PipelineSHA != snapshot.SourceSHA || !gitlabBranch(snapshot.SourceBranch) || snapshot.SourceBranch == snapshot.TargetBranch || snapshot.Evidence == "" || len(snapshot.Evidence) > 16<<10 || !json.Valid([]byte(snapshot.Evidence)) || snapshot.ExpiresAt <= time.Now().UnixNano() {
		return GitLabOperation{}, ErrGitLabDenied
	}
	var op GitLabOperation
	err := s.WithTx(ctx, func(ctx context.Context) error {
		g, err := s.gitlabCheckGrant(ctx, p, "propose", grant)
		if err != nil {
			return err
		}
		if snapshot.ExpiresAt > g.ExpiresAt.UnixNano() {
			return ErrGitLabDenied
		}
		payload, _ := json.Marshal(snapshot)
		op, err = s.reserveGitLabOperation(ctx, p, "propose", request, gitlabHash(payload), grant, snapshot)
		if err != nil {
			return err
		}
		var exists bool
		if err = s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gitlab_proposal_states WHERE operation_id=?)`, op.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO gitlab_proposal_states(operation_id,state,revision) VALUES (?,'pending',1)`, op.ID)
		return err
	})
	if err != nil {
		return GitLabOperation{}, err
	}
	return op, nil
}

// GitLabProposalState returns durable decision state, not authorization.
func (s *Store) GitLabProposalState(ctx context.Context, id string) (string, error) {
	var state string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT state FROM gitlab_proposal_states WHERE operation_id=?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return state, err
}

// StaleGitLabProposal permanently invalidates observed evidence and any approval.
// A later return to the same HEAD cannot revive the proposal. No network is used.
func (s *Store) StaleGitLabProposal(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		state, err := s.GitLabProposalState(ctx, id)
		if err != nil {
			return err
		}
		if state != "pending" {
			return nil
		}
		if _, err = s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_proposal_states SET state='stale',revision=revision+1 WHERE operation_id=? AND state='pending'`, id); err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_approvals SET state='revoked',revision=revision+1 WHERE operation_id=? AND state='approved'`, id)
		return err
	})
}

// DecideGitLabProposal serializes one explicit operator decision. Hash and revision
// bind the decision to the inspected snapshot. Approval is not merge execution.
func (s *Store) DecideGitLabProposal(ctx context.Context, id, session, hash, decision string, revision int64, expires time.Time, policy GitLabApproverPolicy) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		op, err := s.GetGitLabOperation(ctx, id)
		if err != nil {
			return err
		}
		state, err := s.GitLabProposalState(ctx, id)
		if err != nil {
			return err
		}
		if op.PayloadHash != hash || op.Revision != revision || state != "pending" {
			return ErrGitLabConflict
		}
		if _, err = s.GetGitLabApproval(ctx, id); err == nil {
			return ErrGitLabConflict
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if decision == "approve" {
			_, err = s.ApproveGitLabMerge(ctx, id, session, expires, policy)
			return err
		}
		if decision != "reject" {
			return ErrGitLabDenied
		}
		human, e := s.gitlabApprovalAuthority(ctx, op, session, policy)
		if e != nil {
			return e
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_proposal_states SET state='rejected',revision=revision+1,human_id=?,session_hash=? WHERE operation_id=? AND state='pending'`, human, session, id)
		return err
	})
}

// GitLabProposals returns bounded historical records visible to the current user.
// Visibility and the optional swarm filter apply before the page limit. The
// authenticated adapter must still recheck session and access before disclosure.
func (s *Store) GitLabProposals(ctx context.Context, project, swarm, human string) ([]GitLabOperation, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT o.id FROM gitlab_operations o
 JOIN gitlab_proposal_states p ON p.operation_id=o.id
 JOIN swarms sw ON sw.id=o.swarm_id AND sw.project_id=o.project_id
 WHERE o.project_id=? AND (?='' OR o.swarm_id=?) AND sw.state<>'deleted'
 AND EXISTS (SELECT 1 FROM users u WHERE u.id=? AND u.disabled=0 AND
 (u.is_admin=1 OR (EXISTS (SELECT 1 FROM project_members pm WHERE pm.project_id=o.project_id AND pm.user_id=u.id)
 AND EXISTS (SELECT 1 FROM swarm_members sm WHERE sm.swarm_id=o.swarm_id AND sm.user_id=u.id))))
 ORDER BY o.created_at DESC,o.id DESC LIMIT 100`, project, swarm, swarm, human)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]GitLabOperation, 0, len(ids))
	for _, id := range ids {
		op, err := s.GetGitLabOperation(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, op)
	}
	return result, nil
}
