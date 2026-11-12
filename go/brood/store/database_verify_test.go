package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// FEATURE: Read-only proof that a snapshot matches this binary's schema (#220).
// SPEC: Same DDL checks as Open, but no write, no migration, bound to the
// snapshot SHA-256; a different table definition or version is refused.
// IMPLEMENTATION: one-descriptor read, sha256 of those bytes, in-memory
// deserialize and a read-only transaction over schemaModules.
// STATUS: DONE
func currentSnapshot(t *testing.T, mutate string) (string, DatabaseSnapshotMetadata) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source.db")
	s, err := Open(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if mutate != "" {
		db, err := sql.Open("sqlite", dsn(source))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(mutate); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	dir := t.TempDir()
	output := filepath.Join(dir, "snapshot.db")
	meta, err := SnapshotDatabase(context.Background(), source, output)
	if err != nil {
		t.Fatal(err)
	}
	return output, meta
}

func TestVerifySnapshotSchemaAcceptsCurrentSchemaOnReadOnlyDirectory(t *testing.T) {
	path, meta := currentSnapshot(t, "")
	before := databaseBytes(t, path)
	if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(path), 0700) })
	got, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != schemaVersion || got.TargetVersion != schemaVersion || !got.Verified {
		t.Fatalf("verification = %+v", got)
	}
	if string(before) != string(databaseBytes(t, path)) {
		t.Fatal("verification modified the snapshot")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("verification left files: %v", entries)
	}
}

// FEATURE: Schema gate accepts a database the release migrates (#274).
// SPEC: verify applies this binary's migration chain to the hashed snapshot
// bytes in memory and checks the result like Open; the snapshot on disk is
// not written. A newer database (an older image on a newer database), an
// unknown version and a failing step are refused with the store's reason.
// STATUS: DONE
func frozenSnapshot(t *testing.T, version int, change string) (string, DatabaseSnapshotMetadata) {
	t.Helper()
	source := loadSchemaFixture(t, version)
	if change != "" {
		raw := rawDatabase(t, source)
		if _, err := raw.Exec(change); err != nil {
			t.Fatal(err)
		}
		raw.Close()
	}
	output := filepath.Join(t.TempDir(), "snapshot.db")
	meta, err := SnapshotDatabase(context.Background(), source, output)
	if err != nil {
		t.Fatal(err)
	}
	return output, meta
}

func TestVerifyMigratesAnOlderSnapshotOnlyInMemory(t *testing.T) {
	path, meta := frozenSnapshot(t, 26, "")
	if meta.SchemaVersion != 26 {
		t.Fatalf("snapshot version = %d", meta.SchemaVersion)
	}
	before := databaseBytes(t, path)
	if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(path), 0700) })
	got, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256)
	if err != nil {
		t.Fatalf("v26 snapshot refused by the v%d release: %v", schemaVersion, err)
	}
	if want := (DatabaseVerification{SchemaVersion: 26, TargetVersion: schemaVersion, Verified: true}); got != want {
		t.Fatalf("verification = %+v, want %+v", got, want)
	}
	if string(before) != string(databaseBytes(t, path)) {
		t.Fatal("verification modified the snapshot")
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("verification left files: %v", entries)
	}
}

func TestVerifyRefusesNewerUnknownAndFailingMigrations(t *testing.T) {
	for name, tc := range map[string]struct{ change, want string }{
		"newer":   {fmt.Sprintf("PRAGMA user_version=%d", schemaVersion+1), fmt.Sprintf("incompatible database schema: found %d, want %d; the database is newer than this release", schemaVersion+1, schemaVersion)},
		"unknown": {"PRAGMA user_version=25", fmt.Sprintf("incompatible database schema: found 25, want %d; no migration from version 25", schemaVersion)},
		"step":    {"CREATE INDEX swarm_requests_swarm ON swarm_requests(state)", "incompatible database schema: migration from version 26 to 27 failed"},
	} {
		t.Run(name, func(t *testing.T) {
			path, meta := frozenSnapshot(t, 26, tc.change)
			before := databaseBytes(t, path)
			got, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got.Verified {
				t.Fatalf("verify = %+v, %v; want refusal %q", got, err, tc.want)
			}
			if string(before) != string(databaseBytes(t, path)) {
				t.Fatal("refused verification modified the snapshot")
			}
		})
	}
}

func TestVerifySnapshotSchemaRefusesDifferentSchemaHashAndVersion(t *testing.T) {
	path, meta := currentSnapshot(t, "ALTER TABLE swarms ADD COLUMN legacy_extra TEXT")
	if _, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Fatalf("different table accepted: %v", err)
	}
	path, meta = currentSnapshot(t, "")
	if _, err := VerifyDatabaseSchema(context.Background(), path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong hash accepted")
	}
	if _, err := VerifyDatabaseSchema(context.Background(), path, "not-a-hash"); err == nil {
		t.Fatal("malformed hash accepted")
	}
	other, otherMeta := currentSnapshot(t, "PRAGMA user_version=15")
	if _, err := VerifyDatabaseSchema(context.Background(), other, otherMeta.SHA256); err == nil {
		t.Fatal("other schema version accepted")
	}
	if _, err := VerifyDatabaseSchema(context.Background(), filepath.Join(t.TempDir(), "missing.db"), meta.SHA256); err == nil {
		t.Fatal("missing file accepted")
	}
}

// FEATURE: The verified bytes are the hashed bytes (#220).
// SPEC: Verification reads the snapshot once, hashes those bytes and opens
// exactly them as an in-memory database; the path is never handed to SQLite,
// so replacing the file or adding WAL/journal sidecars cannot change the verdict.
// RESULT: tests below prove it for tamper, replacement mid-verify and sidecars.
func TestVerifyBindsDigestToTheVerifiedBytesAcrossFileReplacement(t *testing.T) {
	path, meta := currentSnapshot(t, "")
	foreign, _ := currentSnapshot(t, "ALTER TABLE swarms ADD COLUMN legacy_extra TEXT")
	swapped := false
	got, err := verifyDatabaseSchema(context.Background(), path, meta.SHA256, func() {
		// Подмена после чтения: путь теперь указывает на несовместимую БД.
		if err := os.Rename(foreign, path); err != nil {
			t.Fatal(err)
		}
		swapped = true
	})
	if !swapped {
		t.Fatal("replacement hook did not run")
	}
	if err != nil || !got.Verified {
		t.Fatalf("verdict must describe the hashed bytes, not the replacement: %+v, %v", got, err)
	}
	// Подменённый файл при новой проверке отвергается по digest.
	if _, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256); err == nil {
		t.Fatal("replacement accepted under the old digest")
	}
}

func TestVerifyRefusesTamperedBytesAndAnySingleBitFlip(t *testing.T) {
	path, meta := currentSnapshot(t, "")
	data := databaseBytes(t, path)
	for _, offset := range []int{0, 60, len(data) / 2, len(data) - 1} {
		flipped := append([]byte(nil), data...)
		flipped[offset] ^= 1
		os.Chmod(path, 0600)
		if err := os.WriteFile(path, flipped, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256); err == nil ||
			!strings.Contains(err.Error(), "sha256") {
			t.Fatalf("flip at %d accepted: %v", offset, err)
		}
	}
}

func TestVerifyIgnoresUnexpectedSidecarsAndDoesNotTouchThem(t *testing.T) {
	path, meta := currentSnapshot(t, "")
	sidecars := map[string][]byte{path + "-wal": []byte("not a wal frame"), path + "-journal": []byte("hot journal"),
		path + "-shm": []byte("shm")}
	for name, content := range sidecars {
		if err := os.WriteFile(name, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256)
	if err != nil || !got.Verified {
		t.Fatalf("sidecars influenced the verdict: %+v, %v", got, err)
	}
	for name, content := range sidecars {
		if now, err := os.ReadFile(name); err != nil || string(now) != string(content) {
			t.Fatalf("sidecar %s was touched: %v", name, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 4 {
		t.Fatalf("verification created files: %v", entries)
	}
}

func TestVerifyRefusesSymlinkAndOversizedSnapshots(t *testing.T) {
	path, meta := currentSnapshot(t, "")
	link := filepath.Join(filepath.Dir(path), "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDatabaseSchema(context.Background(), link, meta.SHA256); err == nil {
		t.Fatal("symlink accepted")
	}
	old := maxVerifyBytes
	maxVerifyBytes = 1024
	defer func() { maxVerifyBytes = old }()
	if _, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized snapshot accepted: %v", err)
	}
}

func TestVerifyRefusesFIFOWithoutBlocking(t *testing.T) {
	_, meta := currentSnapshot(t, "")
	fifo := filepath.Join(t.TempDir(), "pipe.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := VerifyDatabaseSchema(context.Background(), fifo, meta.SHA256)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular") {
			t.Fatalf("fifo accepted or wrong error: %v", err)
		}
	case <-time.After(5 * time.Second):
		// Разблокируем зависший open, чтобы не оставлять goroutine.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
		t.Fatal("verify blocked on a FIFO without a writer")
	}
}

func TestVerifyDoesNotReadPastDeclaredSizeWhenFileGrows(t *testing.T) {
	path, meta := currentSnapshot(t, "")
	old := maxVerifyBytes
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	maxVerifyBytes = info.Size() + 1
	defer func() { maxVerifyBytes = old }()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{0, 0})
	f.Close()
	if _, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256); err == nil {
		t.Fatal("grown snapshot accepted")
	}
}

// Схема main (с #206): снимок базы, созданной этим бинарником, с синтетическими
// строками, проходит весь путь snapshot -> verify без изменения байтов. Та же база,
// чей объект схемы отличается от DDL этого бинарника при том же user_version
// (форма сбоя #220), отвергается: отказ называет объект, но не строки (#220).
const verifySeed = `
INSERT INTO users (id, username, password_hash, is_admin, created_at, updated_at)
	VALUES ('u', 'synthetic-private-user', 'synthetic-private-hash', 1, 'now', 'now');
INSERT INTO projects (id, name, created_at) VALUES ('p', 'synthetic-private-project', 'now');
INSERT INTO swarms (id, project_id, owner_id, name, atespace_name, created_at, updated_at)
	VALUES ('s', 'p', 'u', 'synthetic-private-swarm', 'space', 'now', 'now');
INSERT INTO swarm_members (swarm_id, user_id, role) VALUES ('s', 'u', 'owner');
`

func TestVerifyAcceptsSeededCurrentSchemaAndLeavesBytesUntouched(t *testing.T) {
	path, meta := currentSnapshot(t, verifySeed)
	if meta.Users != 1 || meta.EnabledAdmins != 1 {
		t.Fatalf("seed did not reach the snapshot: %+v", meta)
	}
	before := databaseBytes(t, path)
	got, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256)
	if err != nil {
		t.Fatalf("current schema refused: %v", err)
	}
	if !got.Verified || got.SchemaVersion != schemaVersion {
		t.Fatalf("verification = %+v", got)
	}
	if string(before) != string(databaseBytes(t, path)) {
		t.Fatal("verification modified the snapshot")
	}
}

func TestVerifyRejectsChangedObjectsWithValidHashNamingOnlyTheObject(t *testing.T) {
	for name, tc := range map[string]struct{ mutate, want string }{
		"table":   {"ALTER TABLE swarms ADD COLUMN legacy_extra TEXT", "table swarms differs from current schema"},
		"trigger": {"DROP TRIGGER swarm_graph_immutable", "missing trigger swarm_graph_immutable"},
		"index":   {"DROP INDEX swarms_live_name", "missing index swarms_live_name"},
		"owner":   {"DELETE FROM swarm_members", "swarm primary owner has no owner grant"},
	} {
		t.Run(name, func(t *testing.T) {
			path, meta := currentSnapshot(t, verifySeed+tc.mutate)
			_, err := VerifyDatabaseSchema(context.Background(), path, meta.SHA256)
			// Текст, а не errors.Is: deploy разбирает именно его (FAILURES в existing_install.py).
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema: "+tc.want) {
				t.Fatalf("changed %s accepted or wrong reason: %v", name, err)
			}
			if strings.Contains(err.Error(), "synthetic-private") {
				t.Fatalf("refusal leaks row data: %v", err)
			}
		})
	}
}
