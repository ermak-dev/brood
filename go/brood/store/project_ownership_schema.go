package store

import (
	"context"
	"database/sql"
)

func verifyProjectOwnershipSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectOwnershipSchema)
}

const projectOwnershipSchema = `
CREATE TABLE project_ownership (
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision>0),
 owner_id TEXT REFERENCES users(id) ON DELETE SET NULL,
 updated_by TEXT NOT NULL CHECK(length(updated_by)>0),
 updated_at TEXT NOT NULL CHECK(length(updated_at)>0),
 CHECK(owner_id IS NULL OR length(owner_id)>0)
);
`
