package store

import (
	"context"
	"database/sql"
	"errors"
)

// FindGitLabOperation reads internal intent history by exact principal and request.
// Like GetGitLabOperation it is not authorization, including after revocation.
// A guest adapter must recheck the recorded grant before disclosing any state.
func (s *Store) FindGitLabOperation(ctx context.Context, p GitLabPrincipal, kind, request string) (GitLabOperation, error) {
	var id string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT id FROM gitlab_operations WHERE project_id=? AND swarm_id=? AND atespace_uid=? AND actor_uid=? AND incarnation=? AND kind=? AND request_id=?`, p.ProjectID, p.SwarmID, p.AtespaceUID, p.ActorUID, p.Incarnation, kind, request).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return GitLabOperation{}, ErrNotFound
	}
	if err != nil {
		return GitLabOperation{}, err
	}
	return s.GetGitLabOperation(ctx, id)
}
