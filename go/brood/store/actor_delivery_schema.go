package store

import (
	"context"
	"database/sql"
	"strings"
)

// Текст 44 заморожен для шага 43→44 и неизменных fixtures 44–50.
const actorDeliverySchemaV44 = `CREATE TABLE actor_deliveries (
 id TEXT PRIMARY KEY,
 swarm_id TEXT NOT NULL,
 source_kind TEXT NOT NULL CHECK (source_kind IN ('step','answer')),
 source_id TEXT NOT NULL CHECK (length(source_id) BETWEEN 1 AND 512),
 request_id TEXT NOT NULL,
 attested_commit TEXT NOT NULL,
 actor_uid TEXT NOT NULL,
 incarnation INTEGER NOT NULL CHECK (incarnation>0),
 atespace_uid TEXT NOT NULL CHECK (length(atespace_uid)>0),
 journal_id TEXT NOT NULL CHECK (length(journal_id)=64 AND journal_id NOT GLOB '*[^0-9a-f]*'),
 payload TEXT NOT NULL CHECK (json_valid(payload) AND json_type(payload)='object' AND length(CAST(payload AS BLOB))<=131072),
 probe_only INTEGER NOT NULL CHECK (probe_only IN (0,1)),
 created_at TEXT NOT NULL CHECK (length(created_at)>0),
 UNIQUE (swarm_id,source_kind,source_id),
 CHECK ((source_kind='answer' AND request_id='' AND attested_commit='') OR (source_kind='step' AND length(request_id)>0 AND length(attested_commit)>0))
);
CREATE TABLE actor_delivery_attempts (
 delivery_id TEXT NOT NULL REFERENCES actor_deliveries(id),
 number INTEGER NOT NULL CHECK (number BETWEEN 1 AND 8),
 mode TEXT NOT NULL CHECK (mode IN ('send','probe')),
 created_at TEXT NOT NULL CHECK (length(created_at)>0),
 deadline_at TEXT NOT NULL CHECK (length(deadline_at)>0),
 PRIMARY KEY (delivery_id,number)
);
CREATE TABLE actor_delivery_outcomes (
 delivery_id TEXT NOT NULL,
 number INTEGER NOT NULL,
 outcome TEXT NOT NULL CHECK (outcome IN ('delivered','duplicate','not_sent','unconfirmed')),
 detail TEXT NOT NULL CHECK (length(detail)<=512),
 observed_at TEXT NOT NULL CHECK (length(observed_at)>0),
 PRIMARY KEY (delivery_id,number),
 FOREIGN KEY (delivery_id,number) REFERENCES actor_delivery_attempts(delivery_id,number)
);`

var actorDeliverySchema = strings.ReplaceAll(strings.ReplaceAll(actorDeliverySchemaV44, "source_kind IN ('step','answer')", "source_kind IN ('step','answer','child')"), "source_kind='step' AND", "source_kind IN ('step','child') AND")

var actorDeliveryTriggersV44 = []struct{ name, sql string }{
	{"actor_deliveries_no_update", `CREATE TRIGGER actor_deliveries_no_update BEFORE UPDATE ON actor_deliveries BEGIN SELECT RAISE(ABORT, 'delivery identity is immutable'); END`},
	{"actor_deliveries_no_delete", `CREATE TRIGGER actor_deliveries_no_delete BEFORE DELETE ON actor_deliveries BEGIN SELECT RAISE(ABORT, 'delivery history is immutable'); END`},
	{"actor_deliveries_no_replace", `CREATE TRIGGER actor_deliveries_no_replace BEFORE INSERT ON actor_deliveries WHEN EXISTS(SELECT 1 FROM actor_deliveries WHERE id=NEW.id OR (swarm_id=NEW.swarm_id AND source_kind=NEW.source_kind AND source_id=NEW.source_id)) BEGIN SELECT RAISE(ABORT, 'delivery identity is immutable'); END`},
	{"actor_delivery_attempts_no_update", `CREATE TRIGGER actor_delivery_attempts_no_update BEFORE UPDATE ON actor_delivery_attempts BEGIN SELECT RAISE(ABORT, 'delivery attempt is immutable'); END`},
	{"actor_delivery_attempts_no_delete", `CREATE TRIGGER actor_delivery_attempts_no_delete BEFORE DELETE ON actor_delivery_attempts BEGIN SELECT RAISE(ABORT, 'delivery history is immutable'); END`},
	{"actor_delivery_attempts_insert", `CREATE TRIGGER actor_delivery_attempts_insert BEFORE INSERT ON actor_delivery_attempts WHEN NEW.number<>(SELECT COALESCE(MAX(number),0)+1 FROM actor_delivery_attempts WHERE delivery_id=NEW.delivery_id)
 OR EXISTS(SELECT 1 FROM actor_delivery_outcomes WHERE delivery_id=NEW.delivery_id AND outcome IN ('delivered','duplicate'))
 OR (NEW.mode='send' AND ((SELECT probe_only FROM actor_deliveries WHERE id=NEW.delivery_id)=1 OR EXISTS(SELECT 1 FROM actor_delivery_attempts a LEFT JOIN actor_delivery_outcomes o ON o.delivery_id=a.delivery_id AND o.number=a.number WHERE a.delivery_id=NEW.delivery_id AND a.mode='send' AND (o.outcome IS NULL OR o.outcome<>'not_sent'))))
 BEGIN SELECT RAISE(ABORT, 'delivery history forbids this attempt'); END`},
	{"actor_delivery_outcomes_no_update", `CREATE TRIGGER actor_delivery_outcomes_no_update BEFORE UPDATE ON actor_delivery_outcomes BEGIN SELECT RAISE(ABORT, 'delivery receipt is immutable'); END`},
	{"actor_delivery_outcomes_no_delete", `CREATE TRIGGER actor_delivery_outcomes_no_delete BEFORE DELETE ON actor_delivery_outcomes BEGIN SELECT RAISE(ABORT, 'delivery history is immutable'); END`},
	{"actor_delivery_outcomes_no_replace", `CREATE TRIGGER actor_delivery_outcomes_no_replace BEFORE INSERT ON actor_delivery_outcomes WHEN EXISTS(SELECT 1 FROM actor_delivery_outcomes WHERE delivery_id=NEW.delivery_id AND number=NEW.number) BEGIN SELECT RAISE(ABORT, 'delivery receipt is immutable'); END`},
	{"actor_delivery_outcomes_probe", `CREATE TRIGGER actor_delivery_outcomes_probe BEFORE INSERT ON actor_delivery_outcomes WHEN NEW.outcome IN ('delivered','not_sent') AND EXISTS(SELECT 1 FROM actor_delivery_attempts WHERE delivery_id=NEW.delivery_id AND number=NEW.number AND mode='probe') BEGIN SELECT RAISE(ABORT, 'read-only probe cannot claim a new send or no-send'); END`},
}

var actorDeliveryTriggers = append([]struct{ name, sql string }(nil), actorDeliveryTriggersV44...)

func actorDeliveryFullSchemaV44() string {
	statements := []string{actorDeliverySchemaV44}
	for _, trigger := range actorDeliveryTriggersV44 {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}

// Foreign keys выключены migration runner; дочерняя immutable история остаётся
// на месте, без rename родителя и перепривязки REFERENCES к временной таблице.
func actorDeliveryChildMigration() string {
	table := strings.SplitN(actorDeliverySchema, ";", 2)[0]
	statements := []string{
		"CREATE TEMP TABLE actor_deliveries_copy AS SELECT * FROM actor_deliveries",
		"DROP TABLE actor_deliveries",
		table,
		"INSERT INTO actor_deliveries SELECT * FROM actor_deliveries_copy",
		"DROP TABLE actor_deliveries_copy",
	}
	for _, trigger := range actorDeliveryTriggers[:3] {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}

func actorDeliveryFullSchema() string {
	statements := []string{actorDeliverySchema}
	for _, trigger := range actorDeliveryTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}

func verifyActorDeliverySchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, actorDeliverySchema); err != nil {
		return err
	}
	for _, trigger := range actorDeliveryTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
