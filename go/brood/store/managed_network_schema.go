package store

import (
	"context"
	"database/sql"
)

const managedNetworkSchema = `CREATE TABLE project_managed_networks (
 project_id TEXT PRIMARY KEY NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 policy TEXT NOT NULL CHECK (length(policy) BETWEEN 1 AND 131072 AND json_valid(policy))
);
CREATE TABLE managed_actor_networks (
 actor_uid TEXT PRIMARY KEY NOT NULL CHECK (length(actor_uid) > 0),
 policy TEXT NOT NULL CHECK (length(policy) <= 131072 AND (policy = '' OR json_valid(policy)))
);`
const managedNetworkNoUpdate = `CREATE TRIGGER managed_actor_networks_no_update BEFORE UPDATE ON managed_actor_networks BEGIN SELECT RAISE(ABORT, 'managed actor network snapshot is immutable'); END`
const managedNetworkNoDelete = `CREATE TRIGGER managed_actor_networks_no_delete BEFORE DELETE ON managed_actor_networks BEGIN SELECT RAISE(ABORT, 'managed actor network snapshot is immutable'); END`
const managedNetworkNoReplace = `CREATE TRIGGER managed_actor_networks_no_replace BEFORE INSERT ON managed_actor_networks WHEN EXISTS (SELECT 1 FROM managed_actor_networks WHERE actor_uid=NEW.actor_uid) BEGIN SELECT RAISE(ABORT, 'managed actor network snapshot is immutable'); END`

func managedNetworkFullSchema() string {
	return managedNetworkSchema + managedNetworkNoUpdate + ";" + managedNetworkNoDelete + ";" + managedNetworkNoReplace
}
func verifyManagedNetworkSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, managedNetworkSchema); err != nil {
		return err
	}
	for name, ddl := range map[string]string{"managed_actor_networks_no_update": managedNetworkNoUpdate, "managed_actor_networks_no_delete": managedNetworkNoDelete, "managed_actor_networks_no_replace": managedNetworkNoReplace} {
		if err := checkObject(ctx, tx, "trigger", name, ddl); err != nil {
			return err
		}
	}
	return nil
}
