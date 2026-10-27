package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSwarmRetryIntentSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "brood.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	seedSwarm(t, db, "sw1", "p1", "u1", "example")
	if err := db.SetSwarmProgress(ctx, "sw1", "error", "delete"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetSwarm(ctx, "sw1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "error" || got.Retry != "delete" {
		t.Fatalf("lost durable intent: %+v", got)
	}
}
