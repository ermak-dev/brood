package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNewDatabaseIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brood.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential database mode is %o, want 600", info.Mode().Perm())
	}
}
