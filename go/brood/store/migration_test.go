package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// FEATURE: Schema migrations instead of recreating the installation (#274).
// SPEC: an existing database of version N < schemaVersion is brought to the
// current schema by the ordered steps N→N+1→…, each in its own transaction
// that records its version; the last step verifies the result against the
// DDL registry before it commits. A fresh database is created at the current
// version directly. A newer, unknown or unchained version is refused before
// any change; a failing step rolls back to the version before it.
// IMPLEMENTATION: schemaMigrations, migrationPlan and prepareSchema in
// migration.go; Open and VerifyDatabaseSchema share them.
// STATUS: DONE

func TestMigrationPlanWalksTheChainOrRefusesBeforeAnyChange(t *testing.T) {
	steps := []schemaMigration{{from: 24, sql: "SELECT 24"}, {from: 25, sql: "SELECT 25"}, {from: 26, sql: "SELECT 26"}}
	plan, err := migrationPlan(24, 27, steps)
	if err != nil || len(plan) != 3 || plan[0].from != 24 || plan[1].from != 25 || plan[2].from != 26 {
		t.Fatalf("plan 24->27 = %+v, %v", plan, err)
	}
	if plan, err := migrationPlan(27, 27, steps); err != nil || len(plan) != 0 {
		t.Fatalf("current version plans %+v, %v", plan, err)
	}
	for name, tc := range map[string]struct {
		found int
		steps []schemaMigration
		want  string
	}{
		"newer database":  {28, steps, "incompatible database schema: found 28, want 27; the database is newer than this release"},
		"unknown version": {15, steps, "incompatible database schema: found 15, want 27; no migration from version 15"},
		"empty version":   {0, steps, "incompatible database schema: found 0, want 27; no migration from version 0"},
		"gap in chain":    {24, []schemaMigration{steps[0], steps[2]}, "incompatible database schema: found 24, want 27; no migration from version 25"},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := migrationPlan(tc.found, 27, tc.steps)
			if err == nil || err.Error() != tc.want || !errors.Is(err, errIncompatibleSchema) || plan != nil {
				t.Fatalf("plan = %+v, error = %v, want %q", plan, err, tc.want)
			}
		})
	}
}

// TestSchemaMigrationsFormChainToCurrentVersion: реестр задаёт непрерывную
// цепочка шагов по одной версии, последний ведёт в schemaVersion, и у каждой
// исходной версии есть замороженная база, на которой шаг проверяется.
func TestSchemaMigrationsFormChainToCurrentVersion(t *testing.T) {
	if len(schemaMigrations) == 0 {
		t.Fatal("no migrations: a deployed database of the previous version cannot open")
	}
	for i, step := range schemaMigrations {
		if step.sql == "" && step.data == nil {
			t.Errorf("migration from %d does nothing", step.from)
		}
		if i > 0 && step.from != schemaMigrations[i-1].from+1 {
			t.Errorf("migration from %d follows %d: the chain must be ordered without gaps", step.from, schemaMigrations[i-1].from)
		}
		if _, err := os.Stat(schemaFixturePath(step.from)); err != nil {
			t.Errorf("migration from %d has no frozen database %s: %v", step.from, schemaFixturePath(step.from), err)
		}
	}
	if last := schemaMigrations[len(schemaMigrations)-1]; last.from+1 != schemaVersion {
		t.Fatalf("last migration leads to %d, want schemaVersion %d", last.from+1, schemaVersion)
	}
}

// TestMigrateV26DatabaseToCurrent: база версии 26, записанная кодом той же
// версии, открывается текущим кодом, получает индекс swarm_requests(swarm_id)
// и совпадает со свежей базой объект в объект; строки не меняются, кроме
// колонки execution_at роя, которую дописывает шаг 27→28, и пустых таблиц
// Telegram шага 28→29; внешние ключи, триггеры и WAL работают после миграции.
func TestMigrateV26DatabaseToCurrent(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 26)
	raw := rawDatabase(t, path)
	before, ids := databaseRows(t, raw), swarmIDs(t, raw)
	raw.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v26 database refused: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, schemaVersion)
	}
	fresh := newTestStore(t)
	if got, want := schemaObjects(t, s.db), schemaObjects(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from a fresh one:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(strings.Join(schemaObjects(t, s.db), "\n"), "CREATE INDEX swarm_requests_swarm ON swarm_requests(swarm_id)") {
		t.Fatal("migration did not create swarm_requests_swarm")
	}
	// Шаг 27→28 дописывает рою execution_at; точного момента у роёв базы 26
	// нет: s идёт шагом вне прохода запуска, one и two не запускались.
	before["swarms"] = withExecutionAt(before["swarms"], ids, nil)
	if after, want := databaseRows(t, s.db), withTelegramStep(before); !reflect.DeepEqual(after, want) {
		t.Fatalf("migration changed rows:\n got %v\nwant %v", after, want)
	}
	requireIntact(t, s.db)
	var foreignKeys int
	var journal string
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys after migration = %d, %v", foreignKeys, err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal_mode after migration = %q, %v", journal, err)
	}
	if _, err := s.db.Exec(`INSERT INTO swarm_members VALUES ('missing-swarm', 'u', 'owner')`); !IsForeignKeyViolation(err) {
		t.Fatalf("foreign key not enforced after migration: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM swarm_requests`); err == nil || !strings.Contains(err.Error(), "swarm request is permanent") {
		t.Fatalf("swarm_requests_no_delete trigger did not fire after migration: %v", err)
	}
	// Мигрированная база рабочая: сводка списка роёв читает те же строки.
	summaries, err := s.SwarmSummaries(ctx, []string{"s"})
	if err != nil || summaries["s"].OpenRequests != 1 || summaries["s"].UndeliveredAnswers != 1 {
		t.Fatalf("summary over migrated rows = %+v, %v", summaries, err)
	}
}

// executionHistoryV27: рои базы версии 27 на каждом исполнении, которое
// встречает шаг 27→28. done прошёл два прохода: первый остановлен, второй
// кончен ответом, ведущим в выход, в ту же секунду, что остановка первого
// (текстом «…00Z» позже «…00.25Z», временем раньше). broke кончился
// терминальным отказом актора. halted остановлен на ждущем шаге, cut
// посреди доставки (шаг unconfirmed без finished_at), refused после отказа
// шага, который остановка не закрывает. gate ждёт оператора на первом шаге
// прохода, again идёт вторым проходом, начатым узловым шагом, run идёт с
// шага актора. updated_at у всех позже любого исполнения: его двигали
// записи вне исполнения.
const executionHistoryV27 = `
INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,state,created_at,updated_at,execution) VALUES
	('done','p','u','done','space-done','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','succeeded'),
	('broke','p','u','broke','space-broke','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','failed'),
	('halted','p','u','halted','space-halted','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','stopped'),
	('cut','p','u','cut','space-cut','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','stopped'),
	('refused','p','u','refused','space-refused','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','stopped'),
	('gate','p','u','gate','space-gate','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','waiting'),
	('again','p','u','again','space-again','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','running'),
	('run','p','u','run','space-run','active','2026-11-01T00:00:00Z','2026-11-10T00:00:00Z','running');
INSERT INTO swarm_members VALUES ('done','u','owner'), ('broke','u','owner'), ('halted','u','owner'), ('cut','u','owner'),
	('refused','u','owner'), ('gate','u','owner'), ('again','u','owner'), ('run','u','owner');
INSERT INTO swarm_steps (swarm_id,sequence,node_id,state,started_at,finished_at,outcome,goal,source_commit) VALUES
	('done',1,'ask','stopped',NULL,'2026-11-03T10:00:00Z',NULL,'first','0123456789abcdef0123456789abcdef01234567'),
	('done',2,'ask','answered',NULL,'2026-11-03T10:00:00.25Z',NULL,'second','0123456789abcdef0123456789abcdef01234567'),
	('done',3,'done','completed',NULL,NULL,NULL,NULL,NULL),
	('broke',1,'build','completed','2026-11-04T09:00:00Z','2026-11-04T10:00:00.5Z','fail','goal','0123456789abcdef0123456789abcdef01234567'),
	('halted',1,'ask','answered',NULL,'2026-11-05T09:00:00Z',NULL,'goal','0123456789abcdef0123456789abcdef01234567'),
	('halted',2,'build','stopped',NULL,'2026-11-05T10:00:00Z',NULL,NULL,NULL),
	('cut',1,'ask','answered',NULL,'2026-11-06T09:00:00Z',NULL,'goal','0123456789abcdef0123456789abcdef01234567'),
	('cut',2,'build','unconfirmed','2026-11-06T09:30:00Z',NULL,NULL,NULL,NULL),
	('refused',1,'build','refused','2026-11-06T11:00:00Z','2026-11-06T11:00:01Z',NULL,'goal','0123456789abcdef0123456789abcdef01234567'),
	('gate',1,'ask','waiting',NULL,NULL,NULL,'goal','0123456789abcdef0123456789abcdef01234567'),
	('again',1,'ask','stopped',NULL,'2026-11-07T09:00:00Z',NULL,'first','0123456789abcdef0123456789abcdef01234567'),
	('again',2,'ask','answered',NULL,'2026-11-08T09:00:01Z',NULL,'second','0123456789abcdef0123456789abcdef01234567'),
	('again',3,'build','pending',NULL,NULL,NULL,NULL,NULL),
	('run',1,'build','pending',NULL,NULL,NULL,'goal','0123456789abcdef0123456789abcdef01234567');
INSERT INTO swarm_requests (id,swarm_id,sequence,origin,kind,question,options,state,created_at,answered_at,answered_by,outcome) VALUES
	('done-1','done',1,'node','choice','Ship?',x'5b5d','cancelled','2026-11-03T09:00:00Z',NULL,NULL,NULL),
	('done-2','done',2,'node','choice','Ship?',x'5b5d','answered','2026-11-03T10:00:00.125Z','2026-11-03T10:00:00.25Z','u','success'),
	('halted-1','halted',1,'node','choice','Ship?',x'5b5d','answered','2026-11-05T08:00:00Z','2026-11-05T09:00:00Z','u','success'),
	('cut-1','cut',1,'node','choice','Ship?',x'5b5d','answered','2026-11-06T08:00:00Z','2026-11-06T09:00:00Z','u','success'),
	('gate-1','gate',1,'node','choice','Ship?',x'5b5d','open','2026-11-07T12:00:00.125Z',NULL,NULL,NULL),
	('again-1','again',1,'node','choice','Ship?',x'5b5d','cancelled','2026-11-07T08:00:00Z',NULL,NULL,NULL),
	('again-2','again',2,'node','choice','Ship?',x'5b5d','answered','2026-11-08T09:00:00Z','2026-11-08T09:00:01Z','u','success')`

// executionAtV27: момент, который шаг 27→28 восстанавливает роям
// executionHistoryV27 и фикстуры. Конец прохода succeeded или failed:
// finished_at последнего шага с ним; остановка: finished_at последнего
// шага, если его закрыла остановка; начало идущего прохода: created_at
// узлового запроса его первого шага. Пусто: точного момента нет, s идёт
// шагом вне прохода запуска, one и two не запускались, cut и refused
// остановлены на шаге, который остановка не закрыла, run начат шагом актора.
var executionAtV27 = map[string]string{
	"s": "", "one": "", "two": "",
	"done":    "2026-11-03T10:00:00.25Z",
	"broke":   "2026-11-04T10:00:00.5Z",
	"halted":  "2026-11-05T10:00:00Z",
	"cut":     "",
	"refused": "",
	"gate":    "2026-11-07T12:00:00.125Z",
	"again":   "2026-11-08T09:00:00Z",
	"run":     "",
}

// swarmIDs: ID роёв в порядке rowid, в порядке строк tableRows.
func swarmIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM swarms ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// withExecutionAt: строки swarms после шага 27→28: прежние колонки и
// execution_at в конце, момент из at или NULL.
func withExecutionAt(rows, ids []string, at map[string]string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		value := "NULL"
		if moment := at[ids[i]]; moment != "" {
			value = "'" + moment + "'"
		}
		out[i] = row + "," + value
	}
	return out
}

// TestMigrateV27DatabaseToCurrent: шаг 27→28 добавляет swarms.execution_at и
// заполняет его там, где база 27 знает момент точно (executionAtV27), а
// остальным оставляет NULL. Итог совпадает со свежей базой объект в объект,
// прочие строки не меняются, сводка списка роёв отдаёт восстановленный
// момент.
func TestMigrateV27DatabaseToCurrent(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 27)
	raw := rawDatabase(t, path)
	if _, err := raw.Exec(executionHistoryV27); err != nil {
		t.Fatal(err)
	}
	requireIntact(t, raw)
	before, ids := databaseRows(t, raw), swarmIDs(t, raw)
	raw.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v27 database refused: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, schemaVersion)
	}
	fresh := newTestStore(t)
	if got, want := schemaObjects(t, s.db), schemaObjects(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from a fresh one:\n got %q\nwant %q", got, want)
	}
	rows, err := s.db.Query(`SELECT id, COALESCE(execution_at, '') FROM swarms`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		got[id] = at
	}
	rows.Close()
	if !reflect.DeepEqual(got, executionAtV27) {
		t.Fatalf("backfilled execution_at:\n got %v\nwant %v", got, executionAtV27)
	}
	before["swarms"] = withExecutionAt(before["swarms"], ids, executionAtV27)
	if after, want := databaseRows(t, s.db), withTelegramStep(before); !reflect.DeepEqual(after, want) {
		t.Fatalf("migration changed rows:\n got %v\nwant %v", after, want)
	}
	requireIntact(t, s.db)
	summaries, err := s.SwarmSummaries(ctx, []string{"done", "cut"})
	if err != nil || summaries["done"].ExecutionAt != executionAtV27["done"] || summaries["cut"].ExecutionAt != "" {
		t.Fatalf("summary over migrated rows = %+v, %v", summaries, err)
	}
}

// sessionsV28: веб-сессии базы версии 28, со сроком и без: в замороженной
// базе сессий нет, а шаг 28→29 дописывает их строкам вид сессии.
const sessionsV28 = `INSERT INTO sessions VALUES
	('cookie-u', 'u', '2026-11-12T08:00:00Z', '2026-12-12T08:00:00Z', x'7b7d'),
	('cookie-root', 'root', '2026-11-12T09:00:00Z', NULL, NULL)`

// withTelegramStep: строки базы после шага 28→29: у сессий в конце вид
// 'web', таблицы привязок Telegram и исходящих уведомлений пусты.
func withTelegramStep(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	var sessions []string
	for _, row := range rows["sessions"] {
		sessions = append(sessions, row+",'web'")
	}
	out["sessions"] = sessions
	out["user_telegram_links"], out["telegram_outbox"] = nil, nil
	return out
}

// TestMigrateV28DatabaseToCurrent: шаг 28→29 добавляет вид сессии
// sessions.kind (прежние сессии — web), пустые user_telegram_links и
// telegram_outbox с индексом и ограждениями и триггер отзыва сессии
// telegram. Итог совпадает со свежей базой объект в объект, прочие строки не
// меняются; веб-сессии находятся по хэшу токена, а привязка, её отзыв и
// ограждения outbox работают на мигрированной базе.
func TestMigrateV28DatabaseToCurrent(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 28)
	raw := rawDatabase(t, path)
	if _, err := raw.Exec(sessionsV28); err != nil {
		t.Fatal(err)
	}
	requireIntact(t, raw)
	before := databaseRows(t, raw)
	raw.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v28 database refused: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, schemaVersion)
	}
	fresh := newTestStore(t)
	if got, want := schemaObjects(t, s.db), schemaObjects(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from a fresh one:\n got %q\nwant %q", got, want)
	}
	if after, want := databaseRows(t, s.db), withTelegramStep(before); !reflect.DeepEqual(after, want) {
		t.Fatalf("migration changed rows:\n got %v\nwant %v", after, want)
	}
	requireIntact(t, s.db)
	if sess, err := s.GetSession(ctx, "cookie-u"); err != nil || sess.UserID != "u" || sess.ExpiresAt == nil {
		t.Fatalf("web session after migration = %+v, %v", sess, err)
	}
	link := linkTelegram(t, s, "u", "", 42)
	if link.State != "linked" || !active(t, s, "u", "", 42, 42) {
		t.Fatalf("link on the migrated database = %+v", link)
	}
	if _, err := s.DeleteSessionsOfUser(ctx, "u"); err != nil {
		t.Fatal(err)
	}
	if link, err := s.GetTelegramLink(ctx, "u"); err != nil || link.State != "error" || link.Reason != telegramSessionRevoked {
		t.Fatalf("revocation on the migrated database = %+v, %v", link, err)
	}
	id := queueNotification(t, s, "u", "request_opened")
	outboxSteps(t, s, id, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1`)
	if err := outboxExec(s, id, `state = 'pending', telegram_message_id = NULL`); err == nil || !strings.Contains(err.Error(), "telegram notification is immutable") {
		t.Fatalf("outbox fence after migration: %v", err)
	}
	interrupted := queueNotification(t, s, "u", "pass_started")
	outboxSteps(t, s, interrupted, `attempts = 1`)
	if err := outboxExec(s, interrupted, `attempts = 2`); err == nil || !strings.Contains(err.Error(), attemptFence) {
		t.Fatalf("attempt fence after migration: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM telegram_outbox WHERE id = ?`, interrupted); err == nil || !strings.Contains(err.Error(), "telegram notification is queued or in flight") {
		t.Fatalf("delete fence after migration: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM telegram_outbox WHERE id = ?`, id); err != nil {
		t.Fatalf("retention of a sent notification after migration: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, telegram_message_id, created_at, updated_at)
		VALUES ('u', 'request_opened', 'r', '{}', 'sent', 1, 1, 7, 'now', 'now')`); err == nil || !strings.Contains(err.Error(), "telegram notification is queued pending") {
		t.Fatalf("insert fence after migration: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, kind) VALUES ('other-kind', 'u', 'now', 'other')`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("session kind check after migration: %v", err)
	}
}

// withLinkColumnStep: строки базы после шага, который добавляет колонку
// привязок Telegram с NULL у прежних строк: 29→30 — рой по умолчанию, 30→31 —
// имя собеседника. Прочие строки те же.
func withLinkColumnStep(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	var links []string
	for _, row := range rows["user_telegram_links"] {
		links = append(links, row+",NULL")
	}
	out["user_telegram_links"] = links
	return out
}

// TestMigrateV29DatabaseToCurrent: шаг 29→30 добавляет
// user_telegram_links.default_swarm_id (#284). Итог совпадает со свежей базой
// объект в объект; прежние привязки — linked, с ждущим кодом, с ошибкой —
// остаются как были, без роя по умолчанию, а /use и снятие роя по
// умолчанию вместе с роем работают на мигрированной базе.
func TestMigrateV29DatabaseToCurrent(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 29)
	raw := rawDatabase(t, path)
	requireIntact(t, raw)
	before := databaseRows(t, raw)
	raw.Close()
	if len(before["user_telegram_links"]) != 3 {
		t.Fatalf("the v29 fixture has no links to migrate: %v", before["user_telegram_links"])
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v29 database refused: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, schemaVersion)
	}
	fresh := newTestStore(t)
	if got, want := schemaObjects(t, s.db), schemaObjects(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from a fresh one:\n got %q\nwant %q", got, want)
	}
	if after, want := databaseRows(t, s.db), withLinkColumnStep(withLinkColumnStep(before)); !reflect.DeepEqual(after, want) {
		t.Fatalf("migration changed rows:\n got %v\nwant %v", after, want)
	}
	requireIntact(t, s.db)
	if link, err := s.GetTelegramLink(ctx, "root"); err != nil || link.State != "linked" || link.ChatID != 4242 || link.DefaultSwarmID != "" {
		t.Fatalf("root link after migration = %+v, %v", link, err)
	}
	if link, err := s.SetTelegramSwarm(ctx, "root", "s", allow); err != nil || link.DefaultSwarmID != "s" {
		t.Fatalf("/use on the migrated database = %+v, %v", link, err)
	}
	if _, err := s.SetTelegramSwarm(ctx, "vw", "s", allow); !errors.Is(err, ErrTelegramUnlinked) {
		t.Fatalf("/use of a failed link on the migrated database = %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO user_telegram_links (user_id, state, events, updated_at, default_swarm_id) VALUES ('u', 'unlinked', '[]', 'now', 'missing')`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("default swarm foreign key after migration: %v", err)
	}
}

// TestMigrateV30DatabaseToCurrent: шаг 30→31 добавляет
// user_telegram_links.telegram_username (#294) и заменяет триггер отзыва
// сессии telegram, который теперь снимает и имя. Итог совпадает со свежей
// базой объект в объект; прежние привязки остаются как были, без имени: его
// приносит следующее сообщение чата. На мигрированной базе имя пишется,
// отзыв сессии снимает его вместе с привязкой, а CHECK держит его форму.
func TestMigrateV30DatabaseToCurrent(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 30)
	raw := rawDatabase(t, path)
	requireIntact(t, raw)
	before := databaseRows(t, raw)
	raw.Close()
	if len(before["user_telegram_links"]) != 3 {
		t.Fatalf("the v30 fixture has no links to migrate: %v", before["user_telegram_links"])
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v30 database refused: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, schemaVersion)
	}
	fresh := newTestStore(t)
	if got, want := schemaObjects(t, s.db), schemaObjects(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from a fresh one:\n got %q\nwant %q", got, want)
	}
	if after, want := databaseRows(t, s.db), withLinkColumnStep(before); !reflect.DeepEqual(after, want) {
		t.Fatalf("migration changed rows:\n got %v\nwant %v", after, want)
	}
	requireIntact(t, s.db)
	link, err := s.GetTelegramLink(ctx, "root")
	if err != nil || link.State != "linked" || link.ChatID != 4242 || link.DefaultSwarmID != "s" || link.TelegramUsername != "" {
		t.Fatalf("root link after migration = %+v, %v", link, err)
	}
	session, err := s.TelegramChatSession(ctx, "root", "root_brood_bot", 4242, 4242)
	if err != nil || session == "" {
		t.Fatalf("root session after migration = %q, %v", session, err)
	}
	mustNil(t, s.NoteTelegramUsername(ctx, "root", session, "root_tg"))
	if link, err := s.GetTelegramLink(ctx, "root"); err != nil || link.TelegramUsername != "root_tg" {
		t.Fatalf("username on the migrated database = %+v, %v", link, err)
	}
	if _, err := s.DeleteSessionsOfUser(ctx, "root"); err != nil {
		t.Fatalf("revocation of a link with a username on the migrated database: %v", err)
	}
	if link, err := s.GetTelegramLink(ctx, "root"); err != nil || link.State != "error" || link.Reason != telegramSessionRevoked || link.TelegramUsername != "" {
		t.Fatalf("revocation on the migrated database = %+v, %v", link, err)
	}
	if _, err := s.db.Exec(`UPDATE user_telegram_links SET telegram_username = 'ed_tg' WHERE user_id = 'ed'`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("username check after migration: %v", err)
	}
}

// TestMigrateEveryFrozenVersionToCurrent: каждая поддержанная версия доходит
// цепочкой до текущей схемы, равной свежей базе, с теми же строками в
// таблицах обеих версий.
func TestMigrateEveryFrozenVersionToCurrent(t *testing.T) {
	ctx := context.Background()
	fresh := newTestStore(t)
	want := schemaObjects(t, fresh.db)
	for _, version := range frozenSchemaVersions(t) {
		if version == schemaVersion {
			continue
		}
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := loadSchemaFixture(t, version)
			raw := rawDatabase(t, path)
			requireIntact(t, raw)
			counts := map[string]int{}
			for table, rows := range databaseRows(t, raw) {
				counts[table] = len(rows)
			}
			raw.Close()
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("v%d database refused: %v", version, err)
			}
			defer s.Close()
			if got := schemaObjects(t, s.db); !reflect.DeepEqual(got, want) {
				t.Fatalf("v%d migrated schema differs from a fresh one:\n got %q\nwant %q", version, got, want)
			}
			for table, rows := range databaseRows(t, s.db) {
				if count, ok := counts[table]; ok && count != len(rows) {
					t.Errorf("table %s has %d rows after migration, %d before", table, len(rows), count)
				}
			}
			requireIntact(t, s.db)
		})
	}
}

// TestEachMigrationStepYieldsTheNextFrozenVersion: шаг N→N+1 на замороженной
// базе N даёт ровно объекты замороженной базы N+1, а не текущей схемы. Шаг,
// который берёт DDL у живого реестра, после правки этого DDL следующей
// версией создал бы уже её объекты, и шаг N+1→N+2 лёг бы поверх них; тест
// требует заморозить текст такого шага до правки.
func TestEachMigrationStepYieldsTheNextFrozenVersion(t *testing.T) {
	ctx := context.Background()
	versions := frozenSchemaVersions(t)
	for _, from := range versions {
		if !slices.Contains(versions, from+1) {
			continue
		}
		t.Run(fmt.Sprintf("%d-%d", from, from+1), func(t *testing.T) {
			db := rawDatabase(t, loadSchemaFixture(t, from))
			conn, err := db.Conn(ctx)
			mustNil(t, err)
			mustNil(t, migrateStep(ctx, conn, schemaVersion, schemaMigrations))
			mustNil(t, conn.Close())
			next := rawDatabase(t, loadSchemaFixture(t, from+1))
			if got, want := schemaObjects(t, db), schemaObjects(t, next); !reflect.DeepEqual(got, want) {
				t.Fatalf("step %d→%d differs from %s: freeze the DDL of the step before changing it in the registry\n got %q\nwant %q",
					from, from+1, schemaFixturePath(from+1), got, want)
			}
		})
	}
}

func TestReopeningMigratedDatabaseIsNoOp(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 26)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	objects, rows := schemaObjects(t, s.db), databaseRows(t, s.db)
	s.Close()
	for range 2 {
		reopened, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("migrated database refused on reopen: %v", err)
		}
		var version int
		if err := reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
			t.Fatalf("reopened version = %d, %v", version, err)
		}
		if got := schemaObjects(t, reopened.db); !reflect.DeepEqual(got, objects) {
			t.Fatalf("reopen changed schema:\n got %q\nwant %q", got, objects)
		}
		if got := databaseRows(t, reopened.db); !reflect.DeepEqual(got, rows) {
			t.Fatal("reopen changed rows")
		}
		reopened.Close()
	}
}

// openedConn: закреплённое соединение базы с прагмами Open, как у
// initializeSchema.
func openedConn(t *testing.T, path string) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return db, conn
}

// TestFailedMigrationStepKeepsThePreviousVersion: шаг, упавший в SQL, в
// Go-переносе данных или оставивший нарушение внешнего ключа, откатывается
// целиком: версия 26, объекты и строки прежние, внешние ключи соединения
// снова включены.
func TestFailedMigrationStepKeepsThePreviousVersion(t *testing.T) {
	failure := errors.New("synthetic data transfer failure")
	for name, tc := range map[string]struct {
		step schemaMigration
		want string
	}{
		"sql":          {schemaMigration{from: 26, sql: swarmRequestSwarm + "; INSERT INTO no_such_table VALUES (1)"}, "no such table"},
		"data":         {schemaMigration{from: 26, sql: swarmRequestSwarm, data: func(context.Context, *sql.Tx) error { return failure }}, failure.Error()},
		"foreign keys": {schemaMigration{from: 26, sql: swarmRequestSwarm + "; DELETE FROM users WHERE id = 'u'"}, "foreign key violation in table"},
		// SQL проходит, но итог не равен реестру DDL: без сверки перед фиксацией
		// база записала бы 27, которую не открывает ни новый, ни прежний образ.
		"schema": {schemaMigration{from: 26, sql: "CREATE INDEX swarm_requests_swarm ON swarm_requests(state)"}, "differs"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := loadSchemaFixture(t, 26)
			raw := rawDatabase(t, path)
			objects, rows := schemaObjects(t, raw), databaseRows(t, raw)
			raw.Close()
			db, conn := openedConn(t, path)
			err := prepareSchema(ctx, conn, 27, []schemaMigration{tc.step})
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema: migration from version 26 to 27 failed") ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("failing step error = %v, want migration failure naming %q", err, tc.want)
			}
			var foreignKeys int
			if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatalf("foreign_keys after a failed step = %d, %v", foreignKeys, err)
			}
			conn.Close()
			db.Close()
			if err := assertDatabaseUnchanged(t, path, 26, schemaSnapshotOf(t, path)); err != nil {
				t.Fatal(err)
			}
			after := rawDatabase(t, path)
			if got := schemaObjects(t, after); !reflect.DeepEqual(got, objects) {
				t.Fatalf("failed step changed schema:\n got %q\nwant %q", got, objects)
			}
			if got := databaseRows(t, after); !reflect.DeepEqual(got, rows) {
				t.Fatal("failed step changed rows")
			}
		})
	}
}

func schemaSnapshotOf(t *testing.T, path string) []string {
	t.Helper()
	db := rawDatabase(t, path)
	defer db.Close()
	return schemaSnapshot(t, db)
}

// TestMigrationRecordsTheVersionAfterEachStep: шаги идут по порядку, каждый
// в своей транзакции; упавший второй шаг не откатывает первый, база остаётся
// на версии первого, а повтор продолжает с неё.
func TestMigrationRecordsTheVersionAfterEachStep(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 26)
	_, conn := openedConn(t, path)
	failing := slices.Clone(schemaMigrations)
	failing[1] = schemaMigration{from: 27, sql: "CREATE TABLE migration_probe (x); INSERT INTO no_such_table VALUES (1)"}
	if err := prepareSchema(ctx, conn, schemaVersion, failing); err == nil ||
		!strings.Contains(err.Error(), "migration from version 27 to 28 failed") {
		t.Fatalf("second step error = %v", err)
	}
	var version, probes, indexes int
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT user_version FROM pragma_user_version),
		(SELECT count(*) FROM sqlite_master WHERE name = 'migration_probe'),
		(SELECT count(*) FROM sqlite_master WHERE name = 'swarm_requests_swarm')`).Scan(&version, &probes, &indexes); err != nil {
		t.Fatal(err)
	}
	if version != 27 || probes != 0 || indexes != 1 {
		t.Fatalf("after a failed second step: version %d, probe %d, index %d; want 27, 0, 1", version, probes, indexes)
	}
	// Исправленный второй шаг: настоящий шаг 27→28 и проба; цепочка идёт до
	// текущей версии, потому что последний шаг сверяется с её реестром.
	fixed := slices.Clone(schemaMigrations)
	fixed[1].sql += "; CREATE TABLE migration_probe (x)"
	if err := prepareSchema(ctx, conn, schemaVersion, fixed); err != nil {
		t.Fatalf("resumed chain: %v", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT user_version FROM pragma_user_version),
		(SELECT count(*) FROM sqlite_master WHERE name = 'migration_probe')`).Scan(&version, &probes); err != nil || version != schemaVersion || probes != 1 {
		t.Fatalf("resumed chain: version %d, probe %d, %v", version, probes, err)
	}
}

// TestOpenRefusesNewerUnknownAndBrokenChainsWithoutChanges: база новее
// релиза (старый образ на новой базе), версия без цепочки и шаг, упавший на
// чужом объекте с тем же именем, дают отказ с причиной; файл не меняется.
func TestOpenRefusesNewerUnknownAndBrokenChainsWithoutChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		change  string
		version int
		want    string
	}{
		"newer":   {fmt.Sprintf("PRAGMA user_version=%d", schemaVersion+1), schemaVersion + 1, fmt.Sprintf("found %d, want %d; the database is newer than this release", schemaVersion+1, schemaVersion)},
		"unknown": {"PRAGMA user_version=25", 25, fmt.Sprintf("found 25, want %d; no migration from version 25", schemaVersion)},
		"step":    {"CREATE INDEX swarm_requests_swarm ON swarm_requests(state)", 26, "migration from version 26 to 27 failed"},
	} {
		t.Run(name, func(t *testing.T) {
			path := loadSchemaFixture(t, 26)
			raw := rawDatabase(t, path)
			if _, err := raw.Exec(tc.change); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, raw)
			rows := databaseRows(t, raw)
			raw.Close()
			s, err := Open(context.Background(), path)
			if s != nil {
				s.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open error = %v, want %q", err, tc.want)
			}
			if err := assertDatabaseUnchanged(t, path, tc.version, before); err != nil {
				t.Fatal(err)
			}
			if got := databaseRows(t, rawDatabase(t, path)); !reflect.DeepEqual(got, rows) {
				t.Fatal("refused Open changed rows")
			}
		})
	}
}

// TestConcurrentOpensMigrateOnce: процессы, открывшие базу 26 одновременно,
// не применяют шаг дважды: шаг перечитывает версию под блокировкой записи.
func TestConcurrentOpensMigrateOnce(t *testing.T) {
	path := loadSchemaFixture(t, 26)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), path)
			if err == nil {
				s.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
	db := rawDatabase(t, path)
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version = %d, %v", version, err)
	}
}

// TestSwarmSummaryUsesTheSwarmRequestIndex: ради этого индекса шаг 26→27:
// подсчёты сводки списка роёв (#266) ищут запросы роя по индексу, а не
// просматривают таблицу запросов на каждый рой.
func TestSwarmSummaryUsesTheSwarmRequestIndex(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+swarmSummaryQuery, `["s"]`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	text := strings.Join(plan, "\n")
	if strings.Count(text, "swarm_requests_swarm") != 2 || strings.Contains(text, "SCAN r") {
		t.Fatalf("summary does not search swarm requests by swarm_requests_swarm:\n%s", text)
	}
}
