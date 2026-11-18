package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

var (
	// ErrProjectOwnerDenied means current installation administrator authority is absent.
	ErrProjectOwnerDenied = errors.New("project owner authorization changed or access denied")
	// ErrProjectOwnerConflict means the expected ownership revision is stale.
	ErrProjectOwnerConflict = errors.New("project owner revision changed")
)

// ProjectOwnership is an explicit current project grant, not inferred membership.
// Revision zero and an empty owner mean the project has no assigned owner.
type ProjectOwnership struct {
	ProjectID string
	Revision  int64
	OwnerID   string
	UpdatedBy string
	UpdatedAt string
}

// ProjectOwnership reads a live project's current explicit owner assignment.
func (s *Store) ProjectOwnership(ctx context.Context, projectID string) (ProjectOwnership, error) {
	out := ProjectOwnership{ProjectID: projectID}
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT COALESCE(o.revision,0),COALESCE(o.owner_id,''),COALESCE(o.updated_by,''),COALESCE(o.updated_at,'') FROM projects p LEFT JOIN project_ownership o ON o.project_id=p.id WHERE p.id=?`, projectID).Scan(&out.Revision, &out.OwnerID, &out.UpdatedBy, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectOwnership{}, ErrNotFound
	}
	if err != nil {
		return ProjectOwnership{}, err
	}
	return out, nil
}

// SetProjectOwner grants or revokes ownership using an administrator's exact CAS.
// A new owner must be an enabled current member. An empty username revokes the grant.
// authorize must recheck the request session using the supplied transaction context.
func (s *Store) SetProjectOwner(ctx context.Context, actorID, projectID string, expected int64, username string, authorize func(context.Context) bool) (ProjectOwnership, error) {
	if expected < 0 || expected == math.MaxInt64 {
		return ProjectOwnership{}, ErrProjectOwnerConflict
	}
	var out ProjectOwnership
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrProjectOwnerDenied
		}
		user, err := s.GetUser(ctx, actorID)
		if err != nil || user.Disabled || !user.IsAdmin {
			return ErrProjectOwnerDenied
		}
		current, err := s.ProjectOwnership(ctx, projectID)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrProjectOwnerConflict
		}
		ownerID := ""
		if username != "" {
			owner, err := s.GetUserByUsername(ctx, username)
			if err != nil || owner.Disabled {
				return ErrProjectOwnerDenied
			}
			member, err := s.IsMember(ctx, projectID, owner.ID)
			if err != nil {
				return err
			}
			if !member {
				return ErrProjectOwnerDenied
			}
			ownerID = owner.ID
		}
		out = ProjectOwnership{ProjectID: projectID, Revision: expected + 1, OwnerID: ownerID, UpdatedBy: actorID, UpdatedAt: nowRFC3339()}
		var result sql.Result
		if expected == 0 {
			result, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO project_ownership(project_id,revision,owner_id,updated_by,updated_at) VALUES(?,?,NULLIF(?,''),?,?)`, projectID, out.Revision, ownerID, actorID, out.UpdatedAt)
		} else {
			result, err = s.conn(ctx).ExecContext(ctx, `UPDATE project_ownership SET revision=?,owner_id=NULLIF(?,''),updated_by=?,updated_at=? WHERE project_id=? AND revision=?`, out.Revision, ownerID, actorID, out.UpdatedAt, projectID, expected)
		}
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrProjectOwnerConflict
		}
		saved, err := s.ProjectOwnership(ctx, projectID)
		if err != nil {
			return err
		}
		if saved != out {
			return ErrProjectOwnerConflict
		}
		return nil
	})
	if err != nil {
		return ProjectOwnership{}, err
	}
	return out, nil
}

// CanManageProjectWorkflowPolicy checks current administrator or explicit owner authority.
// Membership alone, ownership of a swarm, and historical policy authorship grant nothing.
func (s *Store) CanManageProjectWorkflowPolicy(ctx context.Context, userID, projectID string) (bool, error) {
	user, err := s.GetUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if user.Disabled {
		return false, nil
	}
	if user.IsAdmin {
		_, err := s.GetProject(ctx, projectID)
		return err == nil, err
	}
	ownership, err := s.ProjectOwnership(ctx, projectID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ownership.OwnerID == "" || ownership.OwnerID != user.ID {
		return false, nil
	}
	return s.IsMember(ctx, projectID, user.ID)
}
