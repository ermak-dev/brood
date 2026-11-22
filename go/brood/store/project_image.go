package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// projectImageSchema — образы проектов: контейнер-шаблон PVE, собранный
// плейбуком проекта в пуле. Воркеры проекта в этом пуле — клоны последнего
// готового образа.
const projectImageSchema = `CREATE TABLE project_images (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id),
    pool_uid TEXT NOT NULL,
    ctid INTEGER NOT NULL CHECK (ctid > 0),
    owner_nonce TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('building', 'ready', 'failed', 'retired')),
    snapshot TEXT NOT NULL,
    playbook TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    log TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
)`

// ProjectImage is one build of a project's worker image in a pool. CTID and
// OwnerNonce identify the owned PVE container that becomes the template.
type ProjectImage struct {
	ID         string
	ProjectID  string
	PoolUID    string
	CTID       int
	OwnerNonce string
	State      string
	Snapshot   string
	Playbook   string
	Error      string
	Log        string
	CreatedAt  string
	UpdatedAt  string
}

const projectImageCols = `id,project_id,pool_uid,ctid,owner_nonce,state,snapshot,playbook,error,log,created_at,updated_at`

func scanProjectImage(row interface{ Scan(...any) error }) (ProjectImage, error) {
	var i ProjectImage
	err := row.Scan(&i.ID, &i.ProjectID, &i.PoolUID, &i.CTID, &i.OwnerNonce, &i.State, &i.Snapshot, &i.Playbook, &i.Error, &i.Log, &i.CreatedAt, &i.UpdatedAt)
	return i, err
}

func verifyProjectImageSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectImageSchema)
}

// CreateProjectImage records a build that is about to start. One build per
// project and pool runs at a time: a second one is ErrProjectImageBusy.
func (s *Store) CreateProjectImage(ctx context.Context, i ProjectImage) (ProjectImage, error) {
	if i.ID == "" || i.ProjectID == "" || i.PoolUID == "" || i.CTID <= 0 || i.OwnerNonce == "" || i.Snapshot == "" || i.Playbook == "" {
		return ProjectImage{}, errors.New("project image identity, container, snapshot and playbook are required")
	}
	i.State, i.CreatedAt = "building", nowRFC3339()
	i.UpdatedAt = i.CreatedAt
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var busy bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_images WHERE project_id=? AND pool_uid=? AND state='building')`, i.ProjectID, i.PoolUID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrProjectImageBusy
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_images (`+projectImageCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, i.ID, i.ProjectID, i.PoolUID, i.CTID, i.OwnerNonce, i.State, i.Snapshot, i.Playbook, "", "", i.CreatedAt, i.UpdatedAt)
		return err
	})
	return i, err
}

// ErrProjectImageBusy means a build of this project in this pool is running.
var ErrProjectImageBusy = errors.New("an image build of this project is already running in this pool")

// GetProjectImage returns one image of the project.
func (s *Store) GetProjectImage(ctx context.Context, projectID, id string) (ProjectImage, error) {
	i, err := scanProjectImage(s.conn(ctx).QueryRowContext(ctx, `SELECT `+projectImageCols+` FROM project_images WHERE id=? AND project_id=?`, id, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return i, fmt.Errorf("%w: project image", ErrNotFound)
	}
	return i, err
}

// ProjectImageByID returns an image by its ID alone, for the lifecycle of a
// worker that recorded it.
func (s *Store) ProjectImageByID(ctx context.Context, id string) (ProjectImage, error) {
	i, err := scanProjectImage(s.conn(ctx).QueryRowContext(ctx, `SELECT `+projectImageCols+` FROM project_images WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return i, fmt.Errorf("%w: project image", ErrNotFound)
	}
	return i, err
}

// ListProjectImages returns the images of a project, newest first.
func (s *Store) ListProjectImages(ctx context.Context, projectID string) ([]ProjectImage, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT `+projectImageCols+` FROM project_images WHERE project_id=? ORDER BY created_at DESC, id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectImage
	for rows.Next() {
		i, err := scanProjectImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// CurrentProjectImage returns the newest ready image of the project in the
// pool, or ErrNotFound when the project has none there.
func (s *Store) CurrentProjectImage(ctx context.Context, projectID, poolUID string) (ProjectImage, error) {
	i, err := scanProjectImage(s.conn(ctx).QueryRowContext(ctx, `SELECT `+projectImageCols+` FROM project_images WHERE project_id=? AND pool_uid=? AND state='ready' ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, poolUID))
	if errors.Is(err, sql.ErrNoRows) {
		return i, fmt.Errorf("%w: project image", ErrNotFound)
	}
	return i, err
}

// SetProjectImageLog replaces the kept tail of the build output.
func (s *Store) SetProjectImageLog(ctx context.Context, id, log string) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE project_images SET log=?, updated_at=? WHERE id=?`, log, nowRFC3339(), id)
	return err
}

// FinishProjectImage moves a building image to ready or failed. A ready image
// retires the earlier ready images of the same project and pool.
func (s *Store) FinishProjectImage(ctx context.Context, id, state, reason string) error {
	if state != "ready" && state != "failed" {
		return errors.New("project image finishes as ready or failed")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		i, err := s.ProjectImageByID(ctx, id)
		if err != nil {
			return err
		}
		if i.State != "building" {
			return fmt.Errorf("project image is %s, not building", i.State)
		}
		if state == "ready" {
			if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE project_images SET state='retired', updated_at=? WHERE project_id=? AND pool_uid=? AND state='ready'`, nowRFC3339(), i.ProjectID, i.PoolUID); err != nil {
				return err
			}
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE project_images SET state=?, error=?, updated_at=? WHERE id=?`, state, reason, nowRFC3339(), id)
		return err
	})
}

// FailBuildingProjectImages marks builds interrupted by a server stop as
// failed. Call only during exclusive server startup.
func (s *Store) FailBuildingProjectImages(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE project_images SET state='failed', error='the server stopped during the build', updated_at=? WHERE state='building'`, nowRFC3339())
	return err
}

// ProjectImageInUse reports whether a live worker was cloned from the image.
func (s *Store) ProjectImageInUse(ctx context.Context, id string) (bool, error) {
	var used bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_actors WHERE phase<>'deleted' AND json_extract(template,'$.URI')=?)`, ProjectImageURI(id)).Scan(&used)
	return used, err
}

// DeleteProjectImage removes the record after the caller destroyed the container.
func (s *Store) DeleteProjectImage(ctx context.Context, id string) error {
	_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM project_images WHERE id=?`, id)
	return err
}

// ProjectHasImages reports whether any image record of the project remains.
func (s *Store) ProjectHasImages(ctx context.Context, projectID string) (bool, error) {
	var any bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_images WHERE project_id=?)`, projectID).Scan(&any)
	return any, err
}

const projectImageURIPrefix = "image:"

// ProjectImageURI is the template URI a worker records when it is a clone of
// the image; ProjectImageFromURI reads the image ID back.
func ProjectImageURI(id string) string { return projectImageURIPrefix + id }

// ProjectImageFromURI returns the image ID of a worker template URI, or
// false for a worker created from the pool's base template.
func ProjectImageFromURI(uri string) (string, bool) {
	if len(uri) > len(projectImageURIPrefix) && uri[:len(projectImageURIPrefix)] == projectImageURIPrefix {
		return uri[len(projectImageURIPrefix):], true
	}
	return "", false
}
