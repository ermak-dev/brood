package store

import (
	"context"
	"maps"
	"reflect"
	"testing"
	"time"
)

func withEmptyDeliveryAndRequestContextTables(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	for _, table := range []string{"actor_deliveries", "actor_delivery_attempts", "actor_delivery_outcomes"} {
		out[table] = nil
	}
	return withEmptyRequestContextAndPersistentActorClass(out)
}

func TestActorDeliveryMigrationRetainsAllVersion43History(t *testing.T) {
	path := loadSchemaFixture(t, 43)
	old := rawDatabase(t, path)
	before := withEmptyRequestContextAndPersistentActorClass(databaseRows(t, old))
	mustNil(t, old.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	after := databaseRows(t, s.db)
	for _, table := range []string{"actor_deliveries", "actor_delivery_attempts", "actor_delivery_outcomes"} {
		if rows, exists := after[table]; !exists || len(rows) != 0 {
			t.Fatalf("migration invented delivery evidence in %s", table)
		}
		delete(after, table)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("delivery migration changed existing source or historical rows")
	}
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("migration reached schema%d, want%d", version, schemaVersion)
	}
}

func TestActorDeliverySchemaRefusesHistoryRewritesAndUnknownResends(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Now().UTC()
	attempt, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, attempt, "unconfirmed", "reply lost", now, allow))
	for _, statement := range []string{
		`UPDATE actor_deliveries SET payload='{}'`,
		`DELETE FROM actor_deliveries`,
		`INSERT OR REPLACE INTO actor_deliveries SELECT * FROM actor_deliveries`,
		`UPDATE actor_delivery_attempts SET mode='probe'`,
		`DELETE FROM actor_delivery_attempts`,
		`INSERT OR REPLACE INTO actor_delivery_attempts SELECT * FROM actor_delivery_attempts`,
		`UPDATE actor_delivery_outcomes SET outcome='not_sent'`,
		`DELETE FROM actor_delivery_outcomes`,
		`INSERT OR REPLACE INTO actor_delivery_outcomes SELECT * FROM actor_delivery_outcomes`,
		`INSERT INTO actor_delivery_attempts SELECT delivery_id,2,'send',created_at,deadline_at FROM actor_delivery_attempts WHERE number=1`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("history fence accepted %s", statement)
		}
	}
	history, err := s.GetActorDeliveryHistory(ctx, intent.ID)
	mustNil(t, err)
	if len(history) != 1 || history[0].Outcome != "unconfirmed" || history[0].Attempt.Mode != "send" {
		t.Fatal("rejected mutations changed retained evidence")
	}
}

func TestActorDeliveryHistoryDoesNotBlockActorIdentityRemoval(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Now().UTC()
	ticket, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, ticket, "delivered", "confirmed", now, allow))
	if _, err := s.db.Exec(`DELETE FROM managed_actors WHERE uid='worker'`); err != nil {
		t.Fatalf("retained delivery blocked actor identity removal: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM swarms WHERE id='s'`); err != nil {
		t.Fatalf("retained delivery blocked source deletion: %v", err)
	}
	retained, err := s.GetActorDelivery(ctx, intent.ID)
	mustNil(t, err)
	if retained != intent {
		t.Fatal("resource removal changed immutable delivery scope")
	}
	history, err := s.GetActorDeliveryHistory(ctx, intent.ID)
	mustNil(t, err)
	if len(history) != 1 || history[0].Outcome != "delivered" {
		t.Fatal("resource removal discarded receipt")
	}
}

func TestActorDeliveryUnconfirmedLegacyAnswerStartsWithProbeOnly(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	_, err := s.RecordSwarmRequestDelivery(ctx, in.SwarmID, in.SourceID, "unconfirmed")
	mustNil(t, err)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	if !intent.ProbeOnly {
		t.Fatal("legacy uncertainty became a new first delivery")
	}
	now := time.Now().UTC()
	attempt, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	if attempt.Mode != "probe" {
		t.Fatal("missing old intent authorized a resend")
	}
}
