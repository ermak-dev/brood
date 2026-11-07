package server

import (
	"context"
	"database/sql"

	"brood.dev/go/brood/store"
)

func insertLegacyPipelineRun(ctx context.Context, ts *testServer, run store.PipelineRun) error {
	db, err := sql.Open("sqlite", ts.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=10000`); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO pipeline_runs(id,swarm_id,owner_id,graph,status) VALUES (?,?,?,?,?)`, run.ID, run.SwarmID, run.OwnerID, run.Graph, run.Status)
	return err
}
