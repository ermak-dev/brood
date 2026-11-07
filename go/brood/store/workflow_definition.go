package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"brood.dev/go/brood/pipeline"
)

// WorkflowDefinition is the current version of a project workflow.
type WorkflowDefinition struct {
	ProjectID   string
	Name        string
	YAML        string
	Version     int64
	PublishedAt sql.NullString
}

const legacyWorkflowDefinitionSchema = `CREATE TABLE workflow_definitions (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    yaml TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    PRIMARY KEY (project_id, name)
)`

const workflowDefinitionSchema = `CREATE TABLE workflow_definitions (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    yaml TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    published_at TEXT, PRIMARY KEY (project_id, name)
)`

func initializeWorkflowDefinitions(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 11 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='workflow_definitions'`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return errors.New("incompatible database schema: unversioned workflow definitions")
		}
		if _, err := tx.ExecContext(ctx, workflowDefinitionSchema+`; PRAGMA user_version=13`); err != nil {
			return fmt.Errorf("extend workflow schema: %w", err)
		}
	} else if version == 12 {
		if err := checkSchema(ctx, tx, legacyWorkflowDefinitionSchema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE workflow_definitions ADD COLUMN published_at TEXT; PRAGMA user_version=13`); err != nil {
			return fmt.Errorf("extend workflow schema: %w", err)
		}
	} else if version != 13 {
		return errors.New("incompatible database schema: unsupported workflow version")
	}
	return checkSchema(ctx, tx, workflowDefinitionSchema)
}

// GetWorkflowDefinition returns a named definition in a project.
func (s *Store) GetWorkflowDefinition(ctx context.Context, projectID, name string) (WorkflowDefinition, error) {
	var d WorkflowDefinition
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT project_id,name,yaml,version,published_at FROM workflow_definitions WHERE project_id=? AND name=?`, projectID, name).Scan(&d.ProjectID, &d.Name, &d.YAML, &d.Version, &d.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// ListWorkflowDefinitions returns current definitions ordered by name.
func (s *Store) ListWorkflowDefinitions(ctx context.Context, projectID string) ([]WorkflowDefinition, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT project_id,name,yaml,version,published_at FROM workflow_definitions WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkflowDefinition{}
	for rows.Next() {
		var d WorkflowDefinition
		if err := rows.Scan(&d.ProjectID, &d.Name, &d.YAML, &d.Version, &d.PublishedAt); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// ErrWorkflowRunsExist indicates that a project has pipeline history.
var ErrWorkflowRunsExist = errors.New("workflow deletion blocked: project has pipeline runs")

// ErrWorkflowLimit indicates that a workflow limit was exceeded.
var ErrWorkflowLimit = errors.New("workflow limit exceeded")

// DeleteWorkflowDefinition deletes a definition only when the project has no pipeline runs.
func (s *Store) DeleteWorkflowDefinition(ctx context.Context, projectID, actorID, name string, authorize func(context.Context) bool) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrProjectSyncDenied
		}
		user, err := s.GetUser(ctx, actorID)
		if err != nil || user.Disabled || !user.IsAdmin {
			return ErrProjectSyncDenied
		}
		if _, err := s.GetWorkflowDefinition(ctx, projectID, name); err != nil {
			return err
		}
		var runs bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pipeline_runs r LEFT JOIN swarms s ON s.id=r.swarm_id WHERE s.project_id=? OR s.id IS NULL)`, projectID).Scan(&runs); err != nil {
			return err
		}
		if runs {
			return ErrWorkflowRunsExist
		}
		_, err = s.conn(ctx).ExecContext(ctx, `DELETE FROM workflow_definitions WHERE project_id=? AND name=?`, projectID, name)
		return err
	})
}

// SyncWorkflowDefinitions atomically upserts supplied definitions without deleting others.
// authorize must check the session using the supplied context, including within a transaction.
// A changed batch requires an active admin; an unchanged batch permits a project member.
func (s *Store) SyncWorkflowDefinitions(ctx context.Context, projectID, actorID string, definitions map[string]string, authorize func(context.Context) bool) (bool, error) {
	if authorize == nil || !authorize(ctx) {
		return false, ErrProjectSyncDenied
	}
	user, err := s.GetUser(ctx, actorID)
	if err != nil || user.Disabled {
		return false, ErrProjectSyncDenied
	}
	if _, err := s.GetProject(ctx, projectID); err != nil {
		return false, err
	}
	if !user.IsAdmin {
		member, err := s.IsMember(ctx, projectID, actorID)
		if err != nil {
			return false, err
		}
		if !member {
			return false, ErrProjectSyncDenied
		}
	}
	if len(definitions) > pipeline.MaxWorkflowDefinitions {
		return false, fmt.Errorf("%w: too many workflow definitions", ErrWorkflowLimit)
	}
	names := make([]string, 0, len(definitions))
	total := 0
	for name, text := range definitions {
		total += len(name) + len(text)
		if len(name) > pipeline.MaxWorkflowNameBytes || len(text) > pipeline.MaxWorkflowDefinitionBytes || total > pipeline.MaxWorkflowBatchBytes {
			return false, fmt.Errorf("%w: workflow definition batch exceeds size limit", ErrWorkflowLimit)
		}
		doc, err := pipeline.Parse(text, name+".yaml")
		if err != nil {
			return false, fmt.Errorf("workflow %q: %w", name, err)
		}
		if err = doc.Validate(); err != nil {
			return false, fmt.Errorf("workflow %q: %w", name, err)
		}
		if name != doc.Name {
			return false, fmt.Errorf("workflow name %q does not match document name %q", name, doc.Name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	changed := false
	err = s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrProjectSyncDenied
		}
		user, err := s.GetUser(ctx, actorID)
		if err != nil || user.Disabled {
			return ErrProjectSyncDenied
		}
		if _, err = s.GetProject(ctx, projectID); err != nil {
			return err
		}
		existing, err := s.ListWorkflowDefinitions(ctx, projectID)
		if err != nil {
			return err
		}
		merged := make(map[string]string, len(existing)+len(definitions))
		for _, d := range existing {
			merged[d.Name] = d.YAML
		}
		for name, text := range definitions {
			merged[name] = text
		}
		if len(merged) > pipeline.MaxWorkflowDefinitions {
			return fmt.Errorf("%w: too many workflow definitions", ErrWorkflowLimit)
		}
		mergedBytes := 0
		for name, text := range merged {
			mergedBytes += len(name) + len(text)
			if mergedBytes > pipeline.MaxWorkflowBatchBytes {
				return fmt.Errorf("%w: workflow definition batch exceeds size limit", ErrWorkflowLimit)
			}
		}
		pending := make([]string, 0, len(names))
		for _, name := range names {
			d, err := s.GetWorkflowDefinition(ctx, projectID, name)
			if errors.Is(err, ErrNotFound) || err == nil && d.YAML != definitions[name] {
				pending = append(pending, name)
			} else if err != nil {
				return err
			}
		}
		if !user.IsAdmin {
			member, err := s.IsMember(ctx, projectID, actorID)
			if err != nil {
				return err
			}
			if !member || len(pending) > 0 {
				return ErrProjectSyncDenied
			}
		}
		for _, name := range pending {
			_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO workflow_definitions(project_id,name,yaml,version,published_at) VALUES (?,?,?,1,?) ON CONFLICT(project_id,name) DO UPDATE SET yaml=excluded.yaml, version=workflow_definitions.version+1, published_at=excluded.published_at`, projectID, name, definitions[name], time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
		}
		changed = len(pending) > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
