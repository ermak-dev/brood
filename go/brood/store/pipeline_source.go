package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/gitorigin"
)

// PipelineSource identifies the immutable source snapshot required by a run.
type PipelineSource struct {
	Origin string
	Commit string
}

// ErrPipelineSourceUnverified means that a bound run cannot dispatch actors yet.
var ErrPipelineSourceUnverified = errors.New("pipeline source checkout is not independently verified")

const pipelineSourceSchema = `CREATE TABLE pipeline_sources (
    run_id TEXT NOT NULL PRIMARY KEY REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    origin TEXT NOT NULL,
    commit_sha TEXT NOT NULL
)`

const pipelineSourceImmutable = `CREATE TRIGGER pipeline_sources_immutable BEFORE UPDATE ON pipeline_sources BEGIN SELECT RAISE(ABORT, 'pipeline source is immutable'); END`
const pipelineSourceNoReplace = `CREATE TRIGGER pipeline_sources_no_replace BEFORE INSERT ON pipeline_sources WHEN EXISTS (SELECT 1 FROM pipeline_sources WHERE run_id=NEW.run_id) BEGIN SELECT RAISE(ABORT, 'pipeline source is immutable'); END`
const pipelineSourceNoDelete = `CREATE TRIGGER pipeline_sources_no_delete BEFORE DELETE ON pipeline_sources WHEN EXISTS (SELECT 1 FROM pipeline_runs WHERE id=OLD.run_id) BEGIN SELECT RAISE(ABORT, 'pipeline source is immutable'); END`
const pipelineRunNoReplace = `CREATE TRIGGER pipeline_runs_no_replace BEFORE INSERT ON pipeline_runs WHEN EXISTS (SELECT 1 FROM pipeline_runs WHERE id=NEW.id) BEGIN SELECT RAISE(ABORT, 'pipeline run already exists'); END`

func initializePipelineSources(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 14 {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name IN ('pipeline_sources','pipeline_sources_immutable','pipeline_sources_no_replace','pipeline_sources_no_delete','pipeline_runs_no_replace')`).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return errors.New("incompatible database schema: unversioned pipeline source objects")
		}
		if _, err := tx.ExecContext(ctx, pipelineSourceSchema+`;`+pipelineSourceImmutable+`;`+pipelineSourceNoReplace+`;`+pipelineSourceNoDelete+`;`+pipelineRunNoReplace+`; PRAGMA user_version=15`); err != nil {
			return fmt.Errorf("extend pipeline source schema: %w", err)
		}
	} else if version != 15 && version != 16 {
		return errors.New("incompatible database schema: unsupported pipeline source version")
	}
	if err := checkSchema(ctx, tx, pipelineSourceSchema); err != nil {
		return err
	}
	for name, expected := range map[string]string{
		"pipeline_sources_immutable":  pipelineSourceImmutable,
		"pipeline_sources_no_replace": pipelineSourceNoReplace,
		"pipeline_sources_no_delete":  pipelineSourceNoDelete,
		"pipeline_runs_no_replace":    pipelineRunNoReplace,
	} {
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&actual); err != nil || normalizeSchemaSQL(actual) != normalizeSchemaSQL(expected) {
			return errors.New("incompatible database schema: pipeline source immutability trigger differs")
		}
	}
	return nil
}

func canonicalPipelineSource(source *PipelineSource) (*PipelineSource, error) {
	if source == nil {
		return nil, errors.New("pipeline source is required for new runs")
	}
	origin, err := gitorigin.Canonical(source.Origin)
	if err != nil {
		return nil, errors.New("invalid pipeline source origin")
	}
	commit, err := hex.DecodeString(source.Commit)
	if err != nil || len(commit) != 20 || source.Commit != strings.ToLower(source.Commit) {
		return nil, errors.New("pipeline source commit must be a full lowercase SHA-1")
	}
	return &PipelineSource{Origin: origin, Commit: source.Commit}, nil
}
