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
