package store

import (
	"encoding/hex"
	"errors"
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

// Триггеры неизменности источника и идентичности запуска оплачены ревью
// (#158, #160); регистрация перенесена в общий список схемы без
// изменений семантики: pipelineSourceFullSchema и verifyPipelineSourcesSchema
// в registry.go (#205).

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
