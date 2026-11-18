package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// FEATURE: Схема 51 расширяет typed delivery, сохраняя всю историю 50.
// SPEC: Ни выдуманных children/receipts, ни потери FK history; fixture через Store API.
func TestChildDeliveryMigrationPreservesEvery50Row(t *testing.T) {
	if schemaVersion < 51 {
		t.Fatal("typed child delivery requires schema51")
	}
	path := loadSchemaFixture(t, 50)
	raw := rawDatabase(t, path)
	// Frozen 50 не содержала delivery rows: добавляем валидную историю её DDL,
	// включая неизвестный send и дочерние FK, чтобы rebuild проверял не пустые таблицы.
	for _, kind := range []string{"step", "answer"} {
		request, commit := "", ""
		if kind == "step" {
			request, commit = "old-request", "old-commit"
		}
		_, err := raw.Exec(`INSERT INTO actor_deliveries VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "old-"+kind, "historical-swarm", kind, "old-source", request, commit, "historical-actor", 1, "old-space", strings.Repeat("d", 64), `{"body":"original bytes"}`, false, "2026-01-01T00:00:00Z")
		mustNil(t, err)
		_, err = raw.Exec(`INSERT INTO actor_delivery_attempts VALUES(?,1,'send','2026-01-01T00:00:00Z','2026-01-01T00:00:01Z')`, "old-"+kind)
		mustNil(t, err)
		if kind == "step" {
			_, err = raw.Exec(`INSERT INTO actor_delivery_outcomes VALUES(?,1,'not_sent','original evidence','2026-01-01T00:00:00Z')`, "old-"+kind)
			mustNil(t, err)
			_, err = raw.Exec(`INSERT INTO actor_delivery_attempts VALUES(?,2,'send','2026-01-01T00:01:00Z','2026-01-01T00:01:01Z')`, "old-"+kind)
			mustNil(t, err)
			_, err = raw.Exec(`INSERT INTO actor_delivery_outcomes VALUES(?,2,'unconfirmed','unknown receipt','2026-01-01T00:01:00Z')`, "old-"+kind)
			mustNil(t, err)
			_, err = raw.Exec(`INSERT INTO actor_delivery_attempts VALUES(?,3,'probe','2026-01-01T00:02:00Z','2026-01-01T00:02:01Z')`, "old-"+kind)
			mustNil(t, err)
			_, err = raw.Exec(`INSERT INTO actor_delivery_outcomes VALUES(?,3,'duplicate','retained receipt','2026-01-01T00:02:00Z')`, "old-"+kind)
			mustNil(t, err)
		}
	}
	requireIntact(t, raw)
	before := withLegacyCreateGrants(t, raw)
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("migration changed old rows or fabricated delivery/children")
	}
	requireIntact(t, s.db)
	fresh := newTestStore(t)
	if !reflect.DeepEqual(schemaObjects(t, s.db), schemaObjects(t, fresh.db)) {
		t.Fatal("migrated catalog differs from fresh")
	}
}

func seedLegacyDeliveryHistoryFixture(t *testing.T, s *Store, request SwarmRequest) {
	t.Helper()
	ctx := context.Background()
	in := ActorDeliveryInput{SwarmID: request.SwarmID, SourceKind: "answer", SourceID: request.ID, ActorUID: "worker", Incarnation: 1, AtespaceUID: "native-s", JournalID: strings.Repeat("a", 64), Payload: `{"to":"worker","body":"use sqlite","from":"operator:u","waitSec":0}`}
	d, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a, err := s.ReserveActorDeliveryAttempt(ctx, d.ID, at, at.Add(time.Second), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, a, "not_sent", "fixture known-not-sent", at, allow))
	a, err = s.ReserveActorDeliveryAttempt(ctx, d.ID, at.Add(time.Minute), at.Add(time.Minute+time.Second), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, a, "unconfirmed", "fixture unknown receipt", at.Add(time.Minute), allow))
}

func seedEphemeralChildDeliveryFixture(t *testing.T, s *Store) {
	t.Helper()
	var id, childID string
	mustNil(t, s.db.QueryRow(`SELECT intent_id FROM ephemeral_child_allocations WHERE actor_uid='fixture-allocated-worker'`).Scan(&id))
	mustNil(t, s.db.QueryRow(`SELECT id FROM swarm_child_tasks WHERE actor_uid='fixture-allocated-worker'`).Scan(&childID))
	intent, err := s.GetEphemeralChildIntent(context.Background(), id)
	mustNil(t, err)
	in := ActorDeliveryInput{SwarmID: intent.Parent.Principal.SwarmID, SourceKind: "child", SourceID: childID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: "fixture-allocated-worker", Incarnation: 1, AtespaceUID: intent.Parent.Principal.AtespaceUID, JournalID: strings.Repeat("c", 64), Payload: `{"to":"fixture-allocated-worker","body":"durable child instruction","waitSec":0}`}
	_, err = prepareChildDelivery(t, s, context.Background(), id, in, childAllow)
	mustNil(t, err)
}

func TestChildDeliveryFrozen51RetainsRunningChildAndPendingInstruction(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 51))
	var count, attempts int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM actor_deliveries d JOIN swarm_child_tasks c ON c.id=d.source_id JOIN ephemeral_child_allocations a ON a.actor_uid=c.actor_uid JOIN ephemeral_child_intents e ON e.id=a.intent_id JOIN ephemeral_child_attestations p ON p.intent_id=e.id WHERE d.source_kind='child' AND c.actor_uid='fixture-allocated-worker' AND c.state='running' AND c.revision=2 AND c.role='' AND c.delivery='' AND c.outcome IS NULL AND d.actor_uid=c.actor_uid AND d.incarnation=c.actor_incarnation AND d.request_id=e.request_id AND d.attested_commit=p.attested_commit AND p.state='attested'`).Scan(&count))
	mustNil(t, db.QueryRow(`SELECT count(*) FROM actor_delivery_attempts a JOIN actor_deliveries d ON d.id=a.delivery_id WHERE d.source_kind='child'`).Scan(&attempts))
	if count != 1 || attempts != 0 {
		t.Fatal("fixture lacks real durable pending instruction/running child")
	}
	requireIntact(t, db)
}
