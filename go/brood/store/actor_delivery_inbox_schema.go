package store

import (
	"context"
	"database/sql"
	"strings"
)

// The selection references the immutable native intent for its full source
// tuple. Custody and the one-shot permit have no link to live inventory and
// cannot be cleared to reopen either delivery path.
const actorDeliveryInboxTables = `CREATE TABLE actor_delivery_inbox_selections (
 delivery_id TEXT NOT NULL PRIMARY KEY REFERENCES actor_deliveries(id),
 receiver_id TEXT NOT NULL CHECK (length(CAST(receiver_id AS BLOB))=64 AND instr(receiver_id,char(0))=0 AND receiver_id NOT GLOB '*[^0-9a-f]*'),
 digest TEXT NOT NULL CHECK (length(CAST(digest AS BLOB))=64 AND instr(digest,char(0))=0 AND digest NOT GLOB '*[^0-9a-f]*'),
 selected_at TEXT NOT NULL CHECK (length(selected_at)>0)
);
CREATE TABLE actor_delivery_inbox_custody (
 delivery_id TEXT NOT NULL PRIMARY KEY REFERENCES actor_delivery_inbox_selections(delivery_id),
 recorded_at TEXT NOT NULL CHECK (length(recorded_at)>0)
);
CREATE TABLE actor_delivery_inbox_permits (
 delivery_id TEXT NOT NULL PRIMARY KEY REFERENCES actor_delivery_inbox_custody(delivery_id),
 permit_id TEXT NOT NULL UNIQUE CHECK (length(permit_id)>0),
 launch_id TEXT NOT NULL CHECK (length(CAST(launch_id AS BLOB)) BETWEEN 1 AND 128 AND instr(launch_id,char(0))=0 AND launch_id NOT GLOB '*[^A-Za-z0-9_.:-]*'),
 reserved_at TEXT NOT NULL CHECK (length(reserved_at)>0)
);`

var actorDeliveryInboxTriggers = []struct{ name, sql string }{
	{"actor_delivery_inbox_selections_insert", `CREATE TRIGGER actor_delivery_inbox_selections_insert BEFORE INSERT ON actor_delivery_inbox_selections
 WHEN NOT EXISTS(SELECT 1 FROM actor_deliveries d WHERE d.id=NEW.delivery_id AND d.probe_only=0 AND d.journal_id<>NEW.receiver_id)
 OR EXISTS(SELECT 1 FROM actor_delivery_attempts WHERE delivery_id=NEW.delivery_id)
 OR EXISTS(SELECT 1 FROM actor_delivery_inbox_selections WHERE delivery_id=NEW.delivery_id)
 BEGIN SELECT RAISE(ABORT, 'delivery history forbids inbox selection'); END`},
	{"actor_delivery_inbox_selections_no_update", `CREATE TRIGGER actor_delivery_inbox_selections_no_update BEFORE UPDATE ON actor_delivery_inbox_selections BEGIN SELECT RAISE(ABORT, 'inbox selection is immutable'); END`},
	{"actor_delivery_inbox_selections_no_delete", `CREATE TRIGGER actor_delivery_inbox_selections_no_delete BEFORE DELETE ON actor_delivery_inbox_selections BEGIN SELECT RAISE(ABORT, 'inbox selection is immutable'); END`},
	{"actor_delivery_attempts_no_inbox", `CREATE TRIGGER actor_delivery_attempts_no_inbox BEFORE INSERT ON actor_delivery_attempts
 WHEN EXISTS(SELECT 1 FROM actor_delivery_inbox_selections WHERE delivery_id=NEW.delivery_id)
 BEGIN SELECT RAISE(ABORT, 'inbox selection forbids native attempts'); END`},
	{"actor_delivery_inbox_custody_insert", `CREATE TRIGGER actor_delivery_inbox_custody_insert BEFORE INSERT ON actor_delivery_inbox_custody
 WHEN NOT EXISTS(SELECT 1 FROM actor_delivery_inbox_selections WHERE delivery_id=NEW.delivery_id)
 OR EXISTS(SELECT 1 FROM actor_delivery_inbox_custody WHERE delivery_id=NEW.delivery_id)
 BEGIN SELECT RAISE(ABORT, 'delivery history forbids inbox custody'); END`},
	{"actor_delivery_inbox_custody_no_update", `CREATE TRIGGER actor_delivery_inbox_custody_no_update BEFORE UPDATE ON actor_delivery_inbox_custody BEGIN SELECT RAISE(ABORT, 'inbox custody is immutable'); END`},
	{"actor_delivery_inbox_custody_no_delete", `CREATE TRIGGER actor_delivery_inbox_custody_no_delete BEFORE DELETE ON actor_delivery_inbox_custody BEGIN SELECT RAISE(ABORT, 'inbox custody is immutable'); END`},
	{"actor_delivery_inbox_permits_insert", `CREATE TRIGGER actor_delivery_inbox_permits_insert BEFORE INSERT ON actor_delivery_inbox_permits
 WHEN NOT EXISTS(SELECT 1 FROM actor_delivery_inbox_custody WHERE delivery_id=NEW.delivery_id)
 OR EXISTS(SELECT 1 FROM actor_delivery_inbox_permits WHERE delivery_id=NEW.delivery_id OR permit_id=NEW.permit_id)
 BEGIN SELECT RAISE(ABORT, 'delivery history forbids inbox dispatch permit'); END`},
	{"actor_delivery_inbox_permits_no_update", `CREATE TRIGGER actor_delivery_inbox_permits_no_update BEFORE UPDATE ON actor_delivery_inbox_permits BEGIN SELECT RAISE(ABORT, 'inbox dispatch permit is immutable'); END`},
	{"actor_delivery_inbox_permits_no_delete", `CREATE TRIGGER actor_delivery_inbox_permits_no_delete BEFORE DELETE ON actor_delivery_inbox_permits BEGIN SELECT RAISE(ABORT, 'inbox dispatch permit is immutable'); END`},
}

func actorDeliveryInboxFullSchema() string {
	var ddl strings.Builder
	ddl.WriteString(actorDeliveryInboxTables)
	for _, trigger := range actorDeliveryInboxTriggers {
		ddl.WriteString("\n" + trigger.sql + ";")
	}
	return ddl.String()
}

func verifyActorDeliveryInboxSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, actorDeliveryInboxTables); err != nil {
		return err
	}
	for _, trigger := range actorDeliveryInboxTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
