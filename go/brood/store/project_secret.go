package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"

	"brood.dev/go/brood/projectsecret"
)

// secretStorageError prevents database diagnostics from reflecting secret inputs.
func secretStorageError(err error) error {
	return opaqueStorageError(err, "project secret storage operation failed")
}

// opaqueStorageError keeps not-found and context outcomes and replaces every
// other database diagnostic with a fixed detail: it may quote secret inputs.
func opaqueStorageError(err error, detail string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New(detail)
}

func (s *Store) projectSecretRevision(ctx context.Context, projectID string) (int64, error) {
	var revision int64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT COALESCE(r.revision,0) FROM projects p LEFT JOIN project_secret_revisions r ON r.project_id=p.id WHERE p.id=?`, projectID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return revision, err
}

// GetProjectSecretMetadata returns sorted names and sizes, never plaintext values.
// Missing projects return ErrNotFound; unconfigured projects have revision zero.
func (s *Store) GetProjectSecretMetadata(ctx context.Context, projectID string) (projectsecret.Metadata, error) {
	meta := projectsecret.Metadata{Files: []projectsecret.FileInfo{}, Vars: []string{}}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		meta.Revision, err = s.projectSecretRevision(ctx, projectID)
		if err != nil {
			return err
		}
		rows, err := s.conn(ctx).QueryContext(ctx, `SELECT name,length(data) FROM project_secret_files WHERE project_id=? ORDER BY name`, projectID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var f projectsecret.FileInfo
			if err = rows.Scan(&f.Name, &f.Size); err != nil {
				break
			}
			meta.Files = append(meta.Files, f)
		}
		rowErr := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if rowErr != nil {
			return rowErr
		}
		rows, err = s.conn(ctx).QueryContext(ctx, `SELECT name FROM project_secret_vars WHERE project_id=? ORDER BY name`, projectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			meta.Vars = append(meta.Vars, name)
		}
		return rows.Err()
	})
	if err != nil {
		return projectsecret.Metadata{}, secretStorageError(err)
	}
	return meta, nil
}

// ReadProjectSecretBundle returns sensitive plaintext for INTERNAL delivery only.
// It performs no authorization: callers must restrict this to machine-authenticated
// project-scoped delivery. Never expose the result through operator read APIs or logs.
func (s *Store) ReadProjectSecretBundle(ctx context.Context, projectID string) (projectsecret.Bundle, error) {
	var bundle projectsecret.Bundle
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		bundle, err = s.readProjectSecretBundle(ctx, projectID)
		return err
	})
	if err != nil {
		return projectsecret.Bundle{}, secretStorageError(err)
	}
	return bundle, nil
}

func (s *Store) readProjectSecretBundle(ctx context.Context, projectID string) (projectsecret.Bundle, error) {
	b := projectsecret.Bundle{Files: []projectsecret.File{}, Vars: map[string]string{}}
	var err error
	b.Revision, err = s.projectSecretRevision(ctx, projectID)
	if err != nil {
		return b, err
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT name,data FROM project_secret_files WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		var f projectsecret.File
		if err = rows.Scan(&f.Name, &f.Data); err != nil {
			break
		}
		b.Files = append(b.Files, f)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	if rowErr != nil {
		return b, rowErr
	}
	rows, err = s.conn(ctx).QueryContext(ctx, `SELECT name,value FROM project_secret_vars WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		var name, value string
		if err = rows.Scan(&name, &value); err != nil {
			break
		}
		b.Vars[name] = value
	}
	rowErr = rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	if rowErr != nil {
		return b, rowErr
	}
	return b, projectsecret.ValidateBundle(b)
}

// changeProjectSecrets validates the merged state under the same immediate transaction
// that applies the batch and revision, preventing lost updates and quota races.
func (s *Store) changeProjectSecrets(ctx context.Context, projectID string, empty bool, merge func(*projectsecret.Bundle), apply func(context.Context) error) error {
	var validationErr error
	err := s.WithTx(ctx, func(ctx context.Context) error {
		b, err := s.readProjectSecretBundle(ctx, projectID)
		if err != nil {
			return err
		}
		if empty {
			return nil
		}
		merge(&b)
		if err := projectsecret.ValidateBundle(b); err != nil {
			validationErr = err
			return err
		}
		if b.Revision == math.MaxInt64 {
			validationErr = errors.New("project secret revision limit reached")
			return validationErr
		}
		// WithTx joins existing transactions; a savepoint keeps a handled batch
		// error from committing partial rows through the caller's transaction.
		if _, err := s.conn(ctx).ExecContext(ctx, `SAVEPOINT project_secret_batch`); err != nil {
			return err
		}
		released := false
		defer func() {
			if !released {
				cleanup := context.WithoutCancel(ctx)
				s.conn(ctx).ExecContext(cleanup, `ROLLBACK TO project_secret_batch`)
				s.conn(ctx).ExecContext(cleanup, `RELEASE project_secret_batch`)
			}
		}()
		if err := apply(ctx); err != nil {
			return err
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_secret_revisions(project_id,revision) VALUES (?,1) ON CONFLICT(project_id) DO UPDATE SET revision=revision+1`, projectID); err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `RELEASE project_secret_batch`)
		released = err == nil
		return err
	})
	if validationErr != nil {
		return validationErr
	}
	return secretStorageError(err)
}

// PutProjectSecretFiles merges a validated batch atomically. Existing names are
// overwritten; a nonempty successful batch increments the revision exactly once.
func (s *Store) PutProjectSecretFiles(ctx context.Context, projectID string, files []projectsecret.File) error {
	if err := projectsecret.ValidateBatch(files, nil); err != nil {
		return err
	}
	return s.changeProjectSecrets(ctx, projectID, len(files) == 0, func(b *projectsecret.Bundle) {
		merged := make(map[string][]byte, len(b.Files)+len(files))
		for _, f := range b.Files {
			merged[f.Name] = f.Data
		}
		for _, f := range files {
			merged[f.Name] = f.Data
		}
		b.Files = []projectsecret.File{}
		for name, data := range merged {
			b.Files = append(b.Files, projectsecret.File{Name: name, Data: data})
		}
		sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Name < b.Files[j].Name })
	}, func(ctx context.Context) error {
		for _, f := range files {
			data := f.Data
			if data == nil {
				data = []byte{}
			}
			if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_secret_files(project_id,name,data) VALUES (?,?,?) ON CONFLICT(project_id,name) DO UPDATE SET data=excluded.data`, projectID, f.Name, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// PutProjectSecretVars merges values verbatim, with the same batch revision semantics.
func (s *Store) PutProjectSecretVars(ctx context.Context, projectID string, vars map[string]string) error {
	if err := projectsecret.ValidateBatch(nil, vars); err != nil {
		return err
	}
	return s.changeProjectSecrets(ctx, projectID, len(vars) == 0, func(b *projectsecret.Bundle) {
		for name, value := range vars {
			b.Vars[name] = value
		}
	}, func(ctx context.Context) error {
		for name, value := range vars {
			if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_secret_vars(project_id,name,value) VALUES (?,?,?) ON CONFLICT(project_id,name) DO UPDATE SET value=excluded.value`, projectID, name, value); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveProjectSecretFiles atomically removes names. Absent names are harmless;
// nonempty successful batches still advance the revision. Empty batches do not.
func (s *Store) RemoveProjectSecretFiles(ctx context.Context, projectID string, names []string) error {
	if len(names) > projectsecret.MaxFiles {
		return errors.New("secret count exceeds limit")
	}
	removed := make(map[string]bool, len(names))
	for _, name := range names {
		if err := projectsecret.ValidateFileName(name); err != nil {
			return err
		}
		removed[name] = true
	}
	return s.changeProjectSecrets(ctx, projectID, len(names) == 0, func(b *projectsecret.Bundle) {
		files := []projectsecret.File{}
		for _, f := range b.Files {
			if !removed[f.Name] {
				files = append(files, f)
			}
		}
		b.Files = files
	}, func(ctx context.Context) error {
		for name := range removed {
			if _, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM project_secret_files WHERE project_id=? AND name=?`, projectID, name); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveProjectSecretVars removes names with the same semantics as file removal.
func (s *Store) RemoveProjectSecretVars(ctx context.Context, projectID string, names []string) error {
	if len(names) > projectsecret.MaxVars {
		return errors.New("secret count exceeds limit")
	}
	removed := make(map[string]bool, len(names))
	for _, name := range names {
		if err := projectsecret.ValidateVar(name, ""); err != nil {
			return err
		}
		removed[name] = true
	}
	return s.changeProjectSecrets(ctx, projectID, len(names) == 0, func(b *projectsecret.Bundle) {
		for name := range removed {
			delete(b.Vars, name)
		}
	}, func(ctx context.Context) error {
		for name := range removed {
			if _, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM project_secret_vars WHERE project_id=? AND name=?`, projectID, name); err != nil {
				return err
			}
		}
		return nil
	})
}
