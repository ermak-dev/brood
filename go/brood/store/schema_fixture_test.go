package store

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Замороженные базы версий схемы (#274): testdata/schema-<N>.sql содержит DDL и
// строки базы версии N, записанные store.Open и API хранилища той же версии.
// Тесты миграций грузят их как развёрнутую базу. Файл версии N пишет
// -update-schema-fixture, пока schemaVersion равна N; после подъёма версии
// он не меняется и остаётся исходной базой шага N→N+1.
var updateSchemaFixture = flag.Bool("update-schema-fixture", false,
	"write testdata/schema-<schemaVersion>.sql from a fresh seeded database")

var schemaFixtureName = regexp.MustCompile(`^schema-(\d+)\.sql$`)

func schemaFixturePath(version int) string {
	return filepath.Join("testdata", fmt.Sprintf("schema-%d.sql", version))
}

// frozenSchemaVersions: версии, для которых есть замороженная база.
func frozenSchemaVersions(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var versions []int
	for _, entry := range entries {
		if match := schemaFixtureName.FindStringSubmatch(entry.Name()); match != nil {
			version, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatal(err)
			}
			versions = append(versions, version)
		}
	}
	sort.Ints(versions)
	return versions
}

// seedSchemaFixture: база текущего кода со строками в таблицах, которые
// задевают миграции: пользователи, проект, рой с графом и шагами, отвеченный
// и открытый запросы оператору, ответ актору с неизвестной доставкой,
// managed-пул и актор. С версии 28 ещё рой one, прошедший проход запуска до
// конца: каталог и repository проекта, источник роя, шаг с задачей и commit
// прохода и момент последнего исполнения роя (#268). С версии 29 ещё
// веб-сессия и привязки Telegram (#279): root привязан через своего бота с
// сессией telegram, ed ждёт подтверждения кода через бот установки и выбрал
// подписки, у vw ошибка привязки. И исходящие уведомления root, записанные
// переходами отправителя, которые пропускают ограждения версии 29 (API
// отправки хранилище версии 29 ещё не несёт): в очереди, попытка в полёте,
// sent, failed, unconfirmed из восстановления при запуске и sent, которое
// удалил срок хранения последним, поэтому sqlite_sequence ушёл дальше
// последнего номера.
func seedSchemaFixture(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, node := requestStore(t)
	answered := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, answered, "u", "", "use sqlite"))
	if _, err := s.RecordSwarmRequestDelivery(ctx, "s", answered.ID, "unconfirmed"); err != nil {
		t.Fatal(err)
	}
	actorRequest(t, s, "confirm", nil)
	mustNil(t, answer(s, node, "u", "success", "ship"))
	seedUser(t, s, "root", "root")
	mustNil(t, s.SetUserAdmin(ctx, "root", true))
	mustNil(t, s.AddMember(ctx, "one", "u"))
	mustNil(t, s.SetProjectRepository(ctx, "one", startOrigin))
	publish(t, s, "one", map[string]string{"approve": startApprove})
	mustStart(t, s, "approve")
	mustNil(t, answer(s, openNodeRequestOf(t, s, 1), "u", "success", "approved"))
	expires := telegramTestTime.Add(30 * 24 * time.Hour)
	mustNil(t, s.CreateSession(ctx, Session{TokenHash: "fixture-web-session", UserID: "u", ExpiresAt: &expires, Data: []byte(`{"csrf":"fixture"}`)}))
	_, err := s.SetTelegramBot(ctx, "root", "100:fixture-token", "root_brood_bot", "")
	mustNil(t, err)
	linkTelegram(t, s, "root", "root_brood_bot", 4242)
	_, err = s.IssueTelegramLinkCode(ctx, "ed", linkCode("fixture"), telegramTestTime, telegramTestTime.Add(time.Hour))
	mustNil(t, err)
	_, err = s.SetTelegramEvents(ctx, "ed", []string{"request_opened", "actor_question"})
	mustNil(t, err)
	_, err = s.RecordTelegramLinkError(ctx, "vw", "the chat blocked the bot", "")
	mustNil(t, err)
	queueNotification(t, s, "root", "request_opened")
	sent := queueNotification(t, s, "root", "request_answered")
	outboxSteps(t, s, sent, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1001`)
	failed := queueNotification(t, s, "root", "pass_started")
	outboxSteps(t, s, failed, `attempts = 1`, `state = 'failed', settled_attempts = 1`)
	interrupted := queueNotification(t, s, "root", "pass_finished")
	outboxSteps(t, s, interrupted, `attempts = 1`)
	mustNil(t, s.RecoverTelegramOutbox(ctx))
	inFlight := queueNotification(t, s, "root", "step_failed")
	outboxSteps(t, s, inFlight, `attempts = 1`)
	retained := queueNotification(t, s, "root", "actor_question")
	outboxSteps(t, s, retained, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1002`)
	_, err = s.db.Exec(`DELETE FROM telegram_outbox WHERE id = ?`, retained)
	mustNil(t, err)
	return s
}

// dumpDatabase пишет базу в порядке sqlite3 .dump: таблицы, их строки, затем
// индексы и триггеры, чтобы загрузка строк не проходила через триггеры.
// Внешние ключи загрузчик не включает, целостность проверяет тест.
func dumpDatabase(t *testing.T, db *sql.DB) string {
	t.Helper()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	var tables, objects []string
	var names []string
	for rows.Next() {
		var kind, name, definition string
		if err := rows.Scan(&kind, &name, &definition); err != nil {
			t.Fatal(err)
		}
		if kind == "table" {
			tables = append(tables, definition)
			names = append(names, name)
		} else {
			objects = append(objects, definition)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "-- Frozen brood database of schema version %d (#274): DDL and rows written by\n", version)
	b.WriteString("-- store.Open and the store API of that version. Generated by\n")
	b.WriteString("-- go test ./brood/store -run TestSchemaFixtureMatchesCurrentSchema -update-schema-fixture\n")
	b.WriteString("-- while schemaVersion was this version. Never edit it by hand.\n")
	b.WriteString("BEGIN;\n")
	for _, definition := range tables {
		b.WriteString(definition + ";\n")
	}
	for _, name := range names {
		for _, row := range tableRows(t, db, name) {
			fmt.Fprintf(&b, "INSERT INTO %q VALUES(%s);\n", name, row)
		}
	}
	// Счётчики AUTOINCREMENT, как в sqlite3 .dump: вставка строк поднимает
	// счётчик только до последнего уцелевшего номера, а номер удалённой
	// строки новой не достаётся. sqlite_sequence при загрузке заводит сама
	// таблица с AUTOINCREMENT, и счётчики пишутся после строк таблиц.
	var sequences bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence')`).Scan(&sequences); err != nil {
		t.Fatal(err)
	}
	if sequences {
		b.WriteString("DELETE FROM sqlite_sequence;\n")
		for _, row := range tableRows(t, db, "sqlite_sequence") {
			fmt.Fprintf(&b, "INSERT INTO sqlite_sequence VALUES(%s);\n", row)
		}
	}
	for _, definition := range objects {
		b.WriteString(definition + ";\n")
	}
	fmt.Fprintf(&b, "PRAGMA user_version=%d;\nCOMMIT;\n", version)
	return b.String()
}

// tableRows: строки таблицы в порядке rowid как SQL-литералы quote().
func tableRows(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	columns, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	var quoted []string
	for columns.Next() {
		var name string
		if err := columns.Scan(&name); err != nil {
			t.Fatal(err)
		}
		quoted = append(quoted, fmt.Sprintf("quote(%q)", name))
	}
	if err := columns.Err(); err != nil {
		t.Fatal(err)
	}
	columns.Close()
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM %q ORDER BY rowid`, strings.Join(quoted, " || ',' || "), table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// databaseRows: строки всех таблиц базы по имени таблицы.
func databaseRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, name := range objectNames(t, db, "table") {
		out[name] = tableRows(t, db, name)
	}
	return out
}

func objectNames(t *testing.T, db *sql.DB, kind string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%' ORDER BY name`, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// schemaObjects: полный перечень объектов схемы: таблицы, индексы (включая
// автоматические), триггеры и представления с их точным текстом.
func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_stat%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, name, table, definition string
		if err := rows.Scan(&kind, &name, &table, &definition); err != nil {
			t.Fatal(err)
		}
		out = append(out, kind+" "+name+" ON "+table+"\n"+definition)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// loadSchemaFixture создаёт базу из замороженного файла версии и возвращает
// путь. Загрузка идёт мимо Open, как файл развёрнутой установки.
func loadSchemaFixture(t *testing.T, version int) string {
	t.Helper()
	data, err := os.ReadFile(schemaFixturePath(version))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("v%d.db", version))
	db := rawDatabase(t, path)
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("load %s: %v", schemaFixturePath(version), err)
	}
	return path
}

// rawDatabase открывает файл базы мимо Open: WAL, без внешних ключей и
// без проверки схемы.
func rawDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// requireIntact: целостность файла и внешних ключей.
func requireIntact(t *testing.T, db *sql.DB) {
	t.Helper()
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violations in the database")
	}
}

// requireOutboxFixture: замороженная база несёт уведомления на каждом шаге
// отправителя — в очереди, попытку в полёте и каждый записанный исход — и
// счётчик AUTOINCREMENT дальше последнего номера: уведомление с последним
// номером удалил срок хранения, и шаг следующей версии обязан номер не
// вернуть.
func requireOutboxFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT state || ':' || attempts || '/' || settled_attempts FROM telegram_outbox ORDER BY id`)
	mustNil(t, err)
	var shapes []string
	for rows.Next() {
		var shape string
		mustNil(t, rows.Scan(&shape))
		shapes = append(shapes, shape)
	}
	mustNil(t, rows.Err())
	rows.Close()
	sort.Strings(shapes)
	if want := []string{"failed:1/1", "pending:0/0", "pending:1/0", "sent:1/1", "unconfirmed:1/1"}; !reflect.DeepEqual(shapes, want) {
		t.Fatalf("fixture notifications = %v, want %v", shapes, want)
	}
	var sequence, last int64
	mustNil(t, db.QueryRow(`SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name = 'telegram_outbox'), 0), (SELECT max(id) FROM telegram_outbox)`).Scan(&sequence, &last))
	if sequence <= last {
		t.Fatalf("fixture outbox sequence %d does not pass the last id %d: a deleted notification's id would be given again", sequence, last)
	}
}

// FEATURE: Frozen database of the current schema version (#274).
// SPEC: testdata/schema-<schemaVersion>.sql loads to exactly the objects a
// fresh Open creates and opens as is. A schema change without a new version
// and a frozen fixture fails here, before a release could ship it.
// STATUS: DONE
func TestSchemaFixtureMatchesCurrentSchema(t *testing.T) {
	if *updateSchemaFixture {
		s := seedSchemaFixture(t)
		if err := os.WriteFile(schemaFixturePath(schemaVersion), []byte(dumpDatabase(t, s.db)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := loadSchemaFixture(t, schemaVersion)
	fixture := rawDatabase(t, path)
	requireIntact(t, fixture)
	fresh := newTestStore(t)
	if got, want := schemaObjects(t, fixture), schemaObjects(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s differs from the current schema: a schema change raises schemaVersion, "+
			"adds the migration from the deployed version and freezes the new version with "+
			"-update-schema-fixture\n got %q\nwant %q", schemaFixturePath(schemaVersion), got, want)
	}
	if rows := databaseRows(t, fixture); len(rows["swarm_requests"]) == 0 || len(rows["swarm_steps"]) == 0 {
		t.Fatalf("fixture carries no swarm rows: %v", rows)
	}
	if rows := databaseRows(t, fixture); len(rows["user_telegram_links"]) == 0 || len(rows["sessions"]) == 0 {
		t.Fatalf("fixture carries no session or Telegram link rows: %v", rows)
	}
	requireOutboxFixture(t, fixture)
	fixture.Close()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("frozen current database refused: %v", err)
	}
	s.Close()
}
