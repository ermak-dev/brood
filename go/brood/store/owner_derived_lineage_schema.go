package store

import (
	"context"
	"database/sql"
	"strings"
)

const ownerManagedActorLineageTable = `CREATE TABLE owner_managed_actor_lineage (
 actor_uid TEXT NOT NULL CHECK(actor_uid!=''),
 incarnation INTEGER NOT NULL CHECK(incarnation>0),
 operation_id TEXT NOT NULL CHECK(operation_id!=''),
 operation TEXT NOT NULL CHECK(operation IN ('reset','start')),
 prior_incarnation INTEGER NOT NULL CHECK(prior_incarnation>0),
 prior_operation_id TEXT NOT NULL CHECK(prior_operation_id!=''),
 origin_id TEXT NOT NULL REFERENCES actor_interventions(id) ON DELETE RESTRICT,
 PRIMARY KEY(actor_uid,incarnation,operation_id),
 CHECK((operation='reset' AND incarnation=prior_incarnation+1 AND operation_id=prior_operation_id)
 OR (operation='start' AND incarnation=prior_incarnation AND operation_id!=prior_operation_id))
)`

var ownerManagedActorLineageObjects = []struct{ name, sql string }{
	{"owner_managed_actor_lineage_insert", `CREATE TRIGGER owner_managed_actor_lineage_insert BEFORE INSERT ON owner_managed_actor_lineage WHEN
 EXISTS(SELECT 1 FROM owner_managed_actor_lineage WHERE actor_uid=NEW.actor_uid AND incarnation=NEW.incarnation AND operation_id=NEW.operation_id)
 OR NOT EXISTS(SELECT 1 FROM actor_interventions r JOIN managed_actors a ON a.uid=r.actor_uid JOIN swarms sw ON sw.id=a.swarm_id
 WHERE r.id=NEW.origin_id AND r.state='completed' AND r.operation='reset'
 AND r.actor_uid=NEW.actor_uid AND r.swarm_id=a.swarm_id AND r.project_id=sw.project_id
 AND a.incarnation=NEW.incarnation AND a.operation_id=NEW.operation_id AND a.operation=NEW.operation
 AND ((NEW.operation='reset' AND a.stage='create') OR (NEW.operation='start' AND a.stage='start'))
 AND ((NEW.operation='reset' AND r.incarnation=NEW.prior_incarnation AND r.id=NEW.prior_operation_id)
 OR EXISTS(SELECT 1 FROM owner_managed_actor_lineage p WHERE p.actor_uid=NEW.actor_uid AND p.incarnation=NEW.prior_incarnation AND p.operation_id=NEW.prior_operation_id AND p.operation='reset' AND p.origin_id=r.id)))
 BEGIN SELECT RAISE(ABORT,'derived owner lineage requires exact completed reset origin and managed transition'); END`},
	{"owner_managed_actor_lineage_no_update", `CREATE TRIGGER owner_managed_actor_lineage_no_update BEFORE UPDATE ON owner_managed_actor_lineage
 BEGIN SELECT RAISE(ABORT,'derived owner lineage is immutable'); END`},
	{"owner_managed_actor_lineage_no_delete", `CREATE TRIGGER owner_managed_actor_lineage_no_delete BEFORE DELETE ON owner_managed_actor_lineage
 BEGIN SELECT RAISE(ABORT,'derived owner lineage is retained'); END`},
}

func ownerManagedActorLineageFullSchema() string {
	statements := []string{ownerManagedActorLineageTable}
	for _, object := range ownerManagedActorLineageObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func verifyOwnerManagedActorLineageSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ownerManagedActorLineageTable); err != nil {
		return err
	}
	for _, object := range ownerManagedActorLineageObjects {
		if err := checkObject(ctx, tx, "trigger", object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}
