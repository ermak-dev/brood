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
    swarm_id TEXT NOT NULL PRIMARY KEY REFERENCES swarms(id) ON DELETE CASCADE,
    origin TEXT NOT NULL,
    commit_sha TEXT NOT NULL
)`

const pipelineSourceImmutable = `CREATE TRIGGER pipeline_sources_immutable BEFORE UPDATE ON pipeline_sources BEGIN SELECT RAISE(ABORT, 'pipeline source is immutable'); END`
const pipelineSourceNoReplace = `CREATE TRIGGER pipeline_sources_no_replace BEFORE INSERT ON pipeline_sources WHEN EXISTS (SELECT 1 FROM pipeline_sources WHERE swarm_id=NEW.swarm_id) BEGIN SELECT RAISE(ABORT, 'pipeline source is immutable'); END`
const pipelineSourceNoDelete = `CREATE TRIGGER pipeline_sources_no_delete BEFORE DELETE ON pipeline_sources WHEN EXISTS (SELECT 1 FROM swarms WHERE id=OLD.swarm_id) BEGIN SELECT RAISE(ABORT, 'pipeline source is immutable'); END`

// Триггеры неизменности источника оплачены ревью (#158, #160); ключ
// перевешан с запуска на рой без изменения смысла (#206): снимок
// источника остаётся fail-closed. Идентичность
// запуска (бывший pipeline_runs_no_replace) защищает неизменность
// снимка графа роя: swarmGraphNoReplace в registry.go.

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
