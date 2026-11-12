package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DatabaseMetadata contains only deployment-safe aggregate information.
// Inspection deliberately accepts older or newer schema versions without migration.
type DatabaseMetadata struct {
	SchemaVersion int64 `json:"schema_version"`
	EnabledAdmins int64 `json:"enabled_admins"`
	Users         int64 `json:"users"`
}

// DatabaseSnapshotMetadata describes a validated, standalone SQLite snapshot.
type DatabaseSnapshotMetadata struct {
	DatabaseMetadata
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// InspectDatabase reads existing SQLite metadata without initializing or migrating.
// The source must be a regular file, not a symlink. Callers should bound ctx and
// mount the source directory (including WAL/SHM), not only the main database file.
func InspectDatabase(ctx context.Context, path string) (DatabaseMetadata, error) {
	db, err := openInspectionDatabase(ctx, path)
	if err != nil {
		return DatabaseMetadata{}, err
	}
	defer db.Close()
	return inspectDatabaseMetadata(ctx, db)
}

func openInspectionDatabase(ctx context.Context, path string) (*sql.DB, error) {
	if ctx.Err() != nil {
		return nil, errors.New("database operation cancelled or timed out")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("source database must exist and be readable")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("source database must be a regular file, not a symlink")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("source database path is invalid")
	}
	// mode=ro запрещает создание и запись main/WAL. Не immutable: он
	// игнорирует WAL и небезопасен при работающем писателе. SHM обслуживает SQLite.
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(absolute)+"?mode=ro&_pragma=busy_timeout(100)&_pragma=trusted_schema(OFF)")
	if err != nil {
		return nil, errors.New("cannot open source database read-only")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("cannot open source database read-only")
	}
	return db, nil
}

func inspectDatabaseMetadata(ctx context.Context, db *sql.DB) (DatabaseMetadata, error) {
	var metadata DatabaseMetadata
	// Один statement читает счётчики и версию из одного snapshot SQLite.
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT user_version FROM pragma_user_version),
		count(*), coalesce(sum(CASE WHEN is_admin = 1 AND disabled = 0 THEN 1 ELSE 0 END), 0)
		FROM users`).Scan(&metadata.SchemaVersion, &metadata.Users, &metadata.EnabledAdmins)
	if err != nil {
		return DatabaseMetadata{}, errors.New("cannot inspect database metadata: unreadable or incompatible schema")
	}
	return metadata, nil
}

// SnapshotDatabase creates an exclusive 0600 output using SQLite VACUUM INTO.
// SQLite reads a consistent live snapshot, including committed WAL transactions,
// through a mode=ro source connection. No Store.Open or migration is performed.
// Output must not exist; its parent must be a trusted directory. The resulting
// standalone database is integrity-checked, synced, and hashed before success.
// A failed operation removes its partial output. Callers should bound ctx.
func SnapshotDatabase(ctx context.Context, path, output string) (DatabaseSnapshotMetadata, error) {
	var result DatabaseSnapshotMetadata
	db, err := openInspectionDatabase(ctx, path)
	if err != nil {
		return result, err
	}
	defer db.Close()
	if _, err := inspectDatabaseMetadata(ctx, db); err != nil {
		return result, err
	}
	if output == "" {
		return result, errors.New("snapshot output path is required")
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return result, errors.New("snapshot output path is invalid")
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if os.IsExist(err) {
		return result, errors.New("snapshot output already exists")
	}
	if err != nil {
		return result, errors.New("cannot create private snapshot output")
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(absolute)
		}
	}()
	// SQLite допускает существующий пустой файл. Резервирование O_EXCL
	// запрещает перезапись; параметр пути не является SQL или shell-кодом.
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", absolute); err != nil {
		return result, errors.New("cannot snapshot database: SQLite backup failed or timed out")
	}
	metadata, err := validateDatabaseSnapshot(ctx, absolute)
	if err != nil {
		return result, err
	}
	if err := file.Sync(); err != nil {
		return result, errors.New("cannot sync snapshot output")
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if ctx.Err() != nil {
			return result, errors.New("database operation cancelled or timed out")
		}
		n, err := file.Read(buffer)
		if n > 0 {
			hash.Write(buffer[:n])
			result.Bytes += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return DatabaseSnapshotMetadata{}, errors.New("cannot hash snapshot output")
		}
	}
	if err := file.Close(); err != nil {
		return DatabaseSnapshotMetadata{}, errors.New("cannot close snapshot output")
	}
	result.DatabaseMetadata = metadata
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	complete = true
	return result, nil
}

func validateDatabaseSnapshot(ctx context.Context, path string) (DatabaseMetadata, error) {
	db, err := openInspectionDatabase(ctx, path)
	if err != nil {
		return DatabaseMetadata{}, err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return DatabaseMetadata{}, errors.New("snapshot integrity validation failed")
	}
	return inspectDatabaseMetadata(ctx, db)
}

// DatabaseVerification is the deployment-safe result of a schema check.
// SchemaVersion is the version of the verified snapshot, TargetVersion the
// version this binary's Open brings it to; they differ when Open migrates.
type DatabaseVerification struct {
	SchemaVersion int64 `json:"schema_version"`
	TargetVersion int64 `json:"target_version"`
	Verified      bool  `json:"verified"`
}

// maxVerifyBytes bounds the in-memory copy. The helper pod limit is 256Mi and
// peak use is about twice the file (read buffer plus the SQLite deserialized
// copy), so 32 MiB leaves room for the runtime.
var maxVerifyBytes int64 = 32 << 20

// VerifyDatabaseSchema proves that the snapshot at path has SHA-256 wantSHA256
// and that this binary's Open would accept exactly those bytes: the same
// migration chain and module checks as initializeSchema. An older version is
// migrated in memory, so a refused migration is refused here, before a rollout.
// The file is opened once without following symlinks, read into memory and
// hashed; SQLite then opens that same buffer as an in-memory database. The path
// is never given to SQLite, so a file replaced after the read, or unexpected
// -wal/-journal/-shm sidecars, cannot change the verdict and nothing on disk is
// created or written. The error names the failing object or version, never
// row data.
func VerifyDatabaseSchema(ctx context.Context, path, wantSHA256 string) (DatabaseVerification, error) {
	return verifyDatabaseSchema(ctx, path, wantSHA256, nil)
}

func verifyDatabaseSchema(ctx context.Context, path, wantSHA256 string, afterRead func()) (DatabaseVerification, error) {
	var result DatabaseVerification
	if len(wantSHA256) != 64 {
		return result, errors.New("expected snapshot sha256 must be 64 hex characters")
	}
	if _, err := hex.DecodeString(wantSHA256); err != nil || wantSHA256 != strings.ToLower(wantSHA256) {
		return result, errors.New("expected snapshot sha256 must be lowercase hex")
	}
	data, err := readSnapshot(ctx, path)
	if err != nil {
		return result, err
	}
	if afterRead != nil {
		afterRead()
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != wantSHA256 {
		return result, errors.New("snapshot sha256 does not match the verified snapshot")
	}
	db, err := sql.Open("sqlite", "file::memory:?_pragma=trusted_schema(OFF)")
	if err != nil {
		return result, errors.New("cannot open in-memory verification database")
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, errors.New("cannot open in-memory verification database")
	}
	defer conn.Close()
	err = conn.Raw(func(driverConn any) error {
		d, ok := driverConn.(interface{ Deserialize([]byte) error })
		if !ok {
			return errors.New("driver cannot open a snapshot from memory")
		}
		return d.Deserialize(data)
	})
	if err != nil {
		return result, errors.New("snapshot bytes are not a loadable SQLite database")
	}
	var version int64
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return result, errors.New("cannot read snapshot schema version")
	}
	// План проверяется до prepareSchema: пустой snapshot (версия 0) Open
	// создал бы заново, а gate обязан его отвергнуть.
	if _, err := migrationPlan(int(version), schemaVersion, schemaMigrations); err != nil {
		return result, err
	}
	// Тот же путь, что у Open (#274): старая версия мигрирует по цепочке,
	// итог сверяется с реестром. Пишется только копия байтов в памяти.
	if err := prepareSchema(ctx, conn, schemaVersion, schemaMigrations); err != nil {
		return result, err
	}
	return DatabaseVerification{SchemaVersion: version, TargetVersion: schemaVersion, Verified: true}, nil
}

// readSnapshot reads a regular, non-symlink file through one descriptor (opened
// non-blocking, so a FIFO is refused instead of hanging), so the
// size check and the bytes come from the same inode.
func readSnapshot(ctx context.Context, path string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, errors.New("database operation cancelled or timed out")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("snapshot must exist, be readable and not be a symlink")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("snapshot must be a regular file")
	}
	if info.Size() > maxVerifyBytes {
		return nil, errors.New("snapshot is too large to verify in memory")
	}
	// Буфер ровно по размеру инода: без удвоения ReadAll. Лишний байт - файл вырос.
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, errors.New("snapshot cannot be read completely within the size limit")
	}
	var extra [1]byte
	if n, _ := file.Read(extra[:]); n != 0 {
		return nil, errors.New("snapshot cannot be read completely within the size limit")
	}
	if ctx.Err() != nil {
		return nil, errors.New("database operation cancelled or timed out")
	}
	return data, nil
}
