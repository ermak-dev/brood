package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// FEATURE: Read-only inspection and consistent private deployment snapshots.
// SPEC: No initialization/migration, WAL-aware snapshots, integrity validation,
// no overwrite, metadata only, cancellation, and preservation of source data.
// IMPLEMENTATION: mode=ro SQLite queries and parameterized VACUUM INTO.
// RESULT: IMPLEMENTED — real WAL, concurrent writes, and rehearsal restore pass.
// STATUS: DONE
func inspectionFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source ?# database.sqlite")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`PRAGMA wal_autocheckpoint=0;
CREATE TABLE users (username TEXT, password_hash TEXT, is_admin INTEGER, disabled INTEGER);
CREATE TABLE private_payload (id INTEGER PRIMARY KEY, token TEXT);
PRAGMA user_version=987;
INSERT INTO users VALUES ('synthetic-admin', 'synthetic-hash', 1, 0), ('disabled-admin', 'hidden', 1, 1), ('regular-user', 'hidden', 0, 0);
INSERT INTO private_payload VALUES (1, 'synthetic-token');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func databaseBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInspectDatabasePreservesUnrecognizedVersionAndWAL(t *testing.T) {
	path, db := inspectionFixture(t)
	before := databaseBytes(t, path)
	walBefore := databaseBytes(t, path+"-wal")
	if len(walBefore) <= 32 {
		t.Fatal("fixture has no noncheckpointed WAL")
	}
	got, err := InspectDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 987 || got.Users != 3 || got.EnabledAdmins != 1 {
		t.Fatalf("metadata = %+v", got)
	}
	if !bytes.Equal(before, databaseBytes(t, path)) || !bytes.Equal(walBefore, databaseBytes(t, path+"-wal")) {
		t.Fatal("inspection modified database or WAL")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 987 {
		t.Fatal("source version changed")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("source permissions changed")
	}
}

func TestSnapshotDatabaseIncludesWALAndPreservesSource(t *testing.T) {
	path, db := inspectionFixture(t)
	before := databaseBytes(t, path)
	walBefore := databaseBytes(t, path+"-wal")
	output := filepath.Join(t.TempDir(), "backup ' ?#.sqlite")
	got, err := SnapshotDatabase(context.Background(), path, output)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 987 || got.Users != 3 || got.EnabledAdmins != 1 {
		t.Fatalf("metadata = %+v", got)
	}
	data := databaseBytes(t, output)
	hash := sha256.Sum256(data)
	if got.SHA256 != hex.EncodeToString(hash[:]) || got.Bytes != int64(len(data)) || got.Bytes == 0 {
		t.Fatalf("snapshot metadata = %+v", got)
	}
	info, _ := os.Stat(output)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot mode = %o", info.Mode().Perm())
	}
	if !bytes.Equal(before, databaseBytes(t, path)) || !bytes.Equal(walBefore, databaseBytes(t, path+"-wal")) {
		t.Fatal("snapshot modified source database or WAL")
	}
	copyDB, err := sql.Open("sqlite", dsn(output))
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var integrity, token string
	if err := copyDB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("snapshot failed integrity check")
	}
	if err := copyDB.QueryRow("SELECT token FROM private_payload WHERE id=1").Scan(&token); err != nil || token != "synthetic-token" {
		t.Fatal("snapshot lost WAL payload")
	}
	var sourceVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&sourceVersion); err != nil || sourceVersion != 987 {
		t.Fatal("source version changed")
	}
}

func TestDatabaseRefusesInvalidSourcesAndExistingOutputs(t *testing.T) {
	path, _ := inspectionFixture(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	link := filepath.Join(dir, "link.sqlite")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt.sqlite")
	if err := os.WriteFile(corrupt, []byte("synthetic-sensitive-invalid-database"), 0600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"", missing, dir, link, corrupt, empty} {
		t.Run(filepath.Base(source), func(t *testing.T) {
			if _, err := InspectDatabase(context.Background(), source); err == nil || strings.Contains(err.Error(), "synthetic-sensitive") {
				t.Fatalf("inspect error = %v", err)
			}
			output := filepath.Join(t.TempDir(), "output.sqlite")
			if _, err := SnapshotDatabase(context.Background(), source, output); err == nil || strings.Contains(err.Error(), "synthetic-sensitive") {
				t.Fatalf("snapshot error = %v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("failed snapshot left output")
			}
		})
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing source was created")
	}
	before := databaseBytes(t, path)
	for _, output := range []string{path, corrupt, empty, link, dir, ""} {
		if _, err := SnapshotDatabase(context.Background(), path, output); err == nil {
			t.Fatalf("accepted existing/invalid output %q", output)
		}
	}
	if !bytes.Equal(before, databaseBytes(t, path)) {
		t.Fatal("source overwritten")
	}
	if string(databaseBytes(t, corrupt)) != "synthetic-sensitive-invalid-database" {
		t.Fatal("existing output overwritten")
	}
}

func TestDatabaseCancellation(t *testing.T) {
	path, _ := inspectionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectDatabase(ctx, path); err == nil {
		t.Fatal("inspection ignored cancellation")
	}
	output := filepath.Join(t.TempDir(), "cancelled.sqlite")
	if _, err := SnapshotDatabase(ctx, path, output); err == nil {
		t.Fatal("snapshot ignored cancellation")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("cancelled snapshot left output")
	}
}

func TestSnapshotDatabaseConcurrentWriter(t *testing.T) {
	path, db := inspectionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var writes atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if _, err := db.ExecContext(ctx, "UPDATE private_payload SET token = token || 'x' WHERE id=1"); err != nil {
				return
			}
			writes.Add(1)
		}
	}()
	defer func() { cancel(); <-done }()
	for writes.Load() == 0 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		output := filepath.Join(t.TempDir(), "concurrent.sqlite")
		got, err := SnapshotDatabase(ctx, path, output)
		if err != nil {
			t.Fatal(err)
		}
		if got.Users != 3 || got.EnabledAdmins != 1 {
			t.Fatal("inconsistent snapshot")
		}
	}
	if writes.Load() == 0 {
		t.Fatal("writer did not commit")
	}
}

func TestDatabaseReadOnlyWALAndStandaloneSnapshot(t *testing.T) {
	path, _ := inspectionFixture(t)
	dir := filepath.Dir(path)
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(file, 0400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(dir, 0700)
		for _, file := range []string{path, path + "-wal", path + "-shm"} {
			os.Chmod(file, 0600)
		}
	})
	metadata, err := InspectDatabase(context.Background(), path)
	if err != nil || metadata.Users != 3 {
		t.Fatalf("read-only WAL inspection: %v", err)
	}
	output := filepath.Join(t.TempDir(), "standalone.sqlite")
	if _, err := SnapshotDatabase(context.Background(), path, output); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(output + suffix); !os.IsNotExist(err) {
			t.Fatalf("snapshot requires sidecar %s", suffix)
		}
	}
	if err := os.Chmod(output, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(output), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(output), 0700) })
	if metadata, err := InspectDatabase(context.Background(), output); err != nil || metadata.Users != 3 {
		t.Fatalf("standalone read-only inspection: %v", err)
	}
}

func TestInspectionConnectionRejectsWritesAndCountsEmptyUsers(t *testing.T) {
	path, writer := inspectionFixture(t)
	if _, err := writer.Exec("DELETE FROM users"); err != nil {
		t.Fatal(err)
	}
	reader, err := openInspectionDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Exec("PRAGMA user_version=1"); err == nil {
		t.Fatal("inspection connection permits source schema writes")
	}
	if _, err := reader.Exec("INSERT INTO users VALUES ('test', 'test', 1, 0)"); err == nil {
		t.Fatal("inspection connection permits source data writes")
	}
	metadata, err := InspectDatabase(context.Background(), path)
	if err != nil || metadata.Users != 0 || metadata.EnabledAdmins != 0 || metadata.SchemaVersion != 987 {
		t.Fatalf("empty metadata = %+v, error = %v", metadata, err)
	}
}

func TestSnapshotDatabaseRehearsalRestore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "current.sqlite")
	source, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.BootstrapAdmin(ctx, "synthetic-admin", "synthetic-hash"); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "restore.sqlite")
	metadata, err := SnapshotDatabase(ctx, path, output)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var count int
	if err := restored.db.QueryRow("SELECT count(*) FROM users WHERE password_hash = 'synthetic-hash'").Scan(&count); err != nil || count != 1 {
		t.Fatal("restore lost administrator hash")
	}
	if metadata.EnabledAdmins != 1 || metadata.Users != 1 {
		t.Fatalf("metadata = %+v", metadata)
	}
}
