package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FEATURE: Internal database CLI helpers operate without server configuration.
// SPEC: Real CLI and SQLite, metadata-only JSON, private WAL-aware snapshots,
// bounded timeout flags, no implicit creation or overwrite, sanitized errors.
// IMPLEMENTATION: Explicit flags dispatch directly to read-only store helpers.
// RESULT: IMPLEMENTED — actual binary works with an empty environment.
// STATUS: DONE
func TestDatabaseCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "brood")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, output)
	}
	path := filepath.Join(t.TempDir(), "synthetic-source.sqlite")
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE users(username TEXT, password_hash TEXT, is_admin INTEGER, disabled INTEGER);
PRAGMA user_version=321;
INSERT INTO users VALUES ('private-synthetic-user', 'private-synthetic-hash', 1, 0), ('private-disabled-user', 'private-synthetic-hash', 1, 1);`)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"database"}, args...)...)
		// No credentials or server settings: these helpers are not server startup.
		cmd.Env = []string{}
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		if strings.Contains(out.String()+stderr.String(), "private-") {
			t.Fatal("CLI leaked private row values")
		}
		return out.String(), stderr.String(), err
	}
	t.Run("inspect", func(t *testing.T) {
		out, stderr, err := invoke("inspect", "--path", path)
		if err != nil || stderr != "" {
			t.Fatalf("inspect: %v, %s", err, stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got["schema_version"] != float64(321) || got["enabled_admins"] != float64(1) || got["users"] != float64(2) {
			t.Fatalf("JSON = %s", out)
		}
	})
	t.Run("snapshot", func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "snapshot.sqlite")
		out, stderr, err := invoke("snapshot", "--path", path, "--output", output, "--timeout", "5s")
		if err != nil || stderr != "" {
			t.Fatalf("snapshot: %v, %s", err, stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if len(got) != 5 || got["schema_version"] != float64(321) || got["enabled_admins"] != float64(1) || got["users"] != float64(2) || got["sha256"] != hex.EncodeToString(hash[:]) || got["bytes"] != float64(len(data)) {
			t.Fatalf("JSON = %s", out)
		}
		info, _ := os.Stat(output)
		if info.Mode().Perm() != 0600 {
			t.Fatal("snapshot not private")
		}
		out, stderr, err = invoke("snapshot", "--path", path, "--output", output)
		if err == nil || out != "" || !strings.Contains(stderr, "output already exists") {
			t.Fatalf("overwrite: %v, %s, %s", err, out, stderr)
		}
	})
	t.Run("verify", func(t *testing.T) {
		// Synthetic non-brood schema: the CLI must refuse it, say nothing about rows.
		hash := strings.Repeat("0", 64)
		for _, args := range [][]string{
			{"verify", "--path", path},
			{"verify", "--path", path, "--sha256", "bad"},
			{"verify", "--path", path, "--sha256", hash},
			{"verify", "--path", path, "--sha256", hash, "--output", "/tmp/private-x"},
		} {
			out, stderr, err := invoke(args...)
			if err == nil || out != "" || stderr == "" {
				t.Fatalf("verify %v: %v, %s, %s", args, err, out, stderr)
			}
		}
	})
	t.Run("prune", func(t *testing.T) {
		// Snapshot retention (#247): the current snapshot is kept, older ones go.
		dir := t.TempDir()
		name := func(c string) string { return filepath.Join(dir, ".brood-snapshot-"+strings.Repeat(c, 32)+".db") }
		current, old := name("a"), name("b")
		for _, file := range []string{current, old} {
			if err := os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		out, stderr, err := invoke("prune", "--current", current, "--keep", "1", "--timeout", "5s")
		if err != nil || stderr != "" {
			t.Fatalf("prune: %v, %s", err, stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got["current"] != filepath.Base(current) || got["kept"] != float64(1) || got["removed"] != float64(1) {
			t.Fatalf("JSON = %s", out)
		}
		if _, err := os.Stat(current); err != nil {
			t.Fatal("current snapshot removed")
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatal("old snapshot kept")
		}
		for _, args := range [][]string{
			{"prune"}, {"prune", "--current", current}, {"prune", "--current", current, "--keep", "0"},
			{"prune", "--keep", "1"}, {"prune", "--current", current, "--keep", "private-invalid-count"},
			{"prune", "--current", current, "--keep", "1", "--path", path},
			{"prune", "--current", filepath.Join(dir, "private-foreign.db"), "--keep", "1"},
		} {
			out, stderr, err := invoke(args...)
			if err == nil || out != "" || stderr == "" {
				t.Fatalf("prune refusal %v: %v, %s, %s", args, err, out, stderr)
			}
		}
		if _, err := os.Stat(current); err != nil {
			t.Fatal("refused prune removed the current snapshot")
		}
	})
	t.Run("refusals", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.sqlite")
		for _, args := range [][]string{
			nil, {"unknown-private-value"}, {"inspect"}, {"snapshot", "--path", path},
			{"inspect", "--path", missing}, {"inspect", "--path", path, "extra-private-value"},
			{"inspect", "--path", path, "--unknown-private-flag"},
			{"inspect", "--path", path, "--timeout", "private-invalid-duration"},
			{"inspect", "--path", path, "--timeout", "0s"},
			{"inspect", "--path", path, "--timeout", "2m"},
		} {
			out, stderr, err := invoke(args...)
			if err == nil || out != "" || stderr == "" {
				t.Fatalf("refusal %v: %v, %s, %s", args, err, out, stderr)
			}
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatal("CLI created missing source")
		}
	})
}
