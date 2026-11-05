package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
)

// ErrProjectSyncDenied indicates that current project sync authority is insufficient.
var ErrProjectSyncDenied = errors.New("project sync authorization changed or access denied")

// ErrProjectSyncIdentityChanged requires resolving and locking the project again.
var ErrProjectSyncIdentityChanged = errors.New("project sync identity changed")

// ProjectDeclaration replaces only explicitly declared optional project properties.
type ProjectDeclaration struct {
	Name        string
	Repository  string
	Description *string
	Capacity    *ProjectCapacity
}

// ProjectSyncResult describes the committed inventory change.
type ProjectSyncResult struct {
	Project
	Repository string
	Created    bool
	Changed    bool
}

// ProjectByName returns the unique inventory project with this name.
func (s *Store) ProjectByName(ctx context.Context, name string) (Project, error) {
	var id string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT id FROM projects WHERE name=?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	return s.GetProject(ctx, id)
}

// SyncProject atomically rechecks authority and compares authoritative rows before writing.
// authorize must validate the request session using the supplied transaction context.
// expectedProjectID is the locked identity, or empty after an absent-name lookup.
// Callers serialize existing-project capacity changes with workload admission first.
func (s *Store) SyncProject(ctx context.Context, actorID, expectedProjectID string, declaration ProjectDeclaration, authorize func(context.Context) bool) (ProjectSyncResult, error) {
	var result ProjectSyncResult
	if strings.TrimSpace(declaration.Name) == "" || !storedOrigin(declaration.Repository) {
		return result, errors.New("invalid project declaration")
	}
	if declaration.Capacity != nil {
		if err := ValidateProjectCapacity(*declaration.Capacity); err != nil {
			return result, err
		}
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrProjectSyncDenied
		}
		user, err := s.GetUser(ctx, actorID)
		if err != nil || user.Disabled {
			return ErrProjectSyncDenied
		}
		project, err := s.ProjectByName(ctx, declaration.Name)
		created := errors.Is(err, ErrNotFound)
		if err != nil && !created {
			return err
		}
		if created && expectedProjectID != "" || !created && project.ID != expectedProjectID {
			return ErrProjectSyncIdentityChanged
		}
		if created {
			if !user.IsAdmin {
				return ErrProjectSyncDenied
			}
			project = Project{ID: NewID(), Name: declaration.Name}
		}
		repository := ""
		description := ""
		var capacity ProjectCapacity
		if !created {
			repository, err = s.ProjectRepository(ctx, project.ID)
			if err != nil {
				return err
			}
			err = s.conn(ctx).QueryRowContext(ctx, `SELECT description FROM command_project_details WHERE project_id=?`, project.ID).Scan(&description)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if declaration.Capacity != nil {
				capacity, err = s.GetProjectCapacity(ctx, project.ID)
				if err != nil {
					return err
				}
			}
		}
		repositoryChanged := repository != declaration.Repository
		descriptionChanged := declaration.Description != nil && description != *declaration.Description
		capacityChanged := declaration.Capacity != nil && !sameProjectCapacity(capacity, *declaration.Capacity)
		changed := created || repositoryChanged || descriptionChanged || capacityChanged
		if !user.IsAdmin {
			member, err := s.IsMember(ctx, project.ID, actorID)
			if err != nil {
				return err
			}
			if changed || !member {
				return ErrProjectSyncDenied
			}
		}
		if created {
			if err := s.CreateProject(ctx, project); err != nil {
				return err
			}
		}
		if repositoryChanged {
			if err := s.SetProjectRepository(ctx, project.ID, declaration.Repository); err != nil {
				return err
			}
		}
		if descriptionChanged {
			if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO command_project_details(project_id,description) VALUES (?,?) ON CONFLICT(project_id) DO UPDATE SET description=excluded.description`, project.ID, *declaration.Description); err != nil {
				return err
			}
		}
		if capacityChanged {
			if err := s.SetProjectCapacity(ctx, project.ID, *declaration.Capacity); err != nil {
				return err
			}
		}
		result = ProjectSyncResult{Project: project, Repository: declaration.Repository, Created: created, Changed: changed}
		return nil
	})
	return result, err
}

func sameProjectCapacity(a, b ProjectCapacity) bool {
	if len(a.WorkerSelector) == 0 {
		a.WorkerSelector = nil
	}
	if len(b.WorkerSelector) == 0 {
		b.WorkerSelector = nil
	}
	return reflect.DeepEqual(a, b)
}
