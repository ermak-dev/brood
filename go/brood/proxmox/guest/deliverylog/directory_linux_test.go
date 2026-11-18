package deliverylog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDirectoryKeepsJournalInPinnedPrivateNamespace(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "current")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	retained := filepath.Join(parent, "retained")
	if err := os.Rename(path, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := OpenDirectory(context.Background(), dir, Scope{ActorUID: "actor", Incarnation: 1}, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Begin(context.Background(), "stable", []byte("work")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(retained, "delivery.jsonl")); err != nil {
		t.Fatal("journal did not use retained directory descriptor")
	}
	if _, err := os.Stat(filepath.Join(path, "delivery.jsonl")); !os.IsNotExist(err) {
		t.Fatal("journal followed replacement pathname")
	}
	if _, err := dir.Stat(); err != nil {
		t.Fatal("journal consumed caller's directory descriptor")
	}
}

func TestOpenDirectoryRefusesSharedNamespace(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0770); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if j, err := OpenDirectory(context.Background(), dir, Scope{ActorUID: "actor", Incarnation: 1}, 2); err == nil {
		j.Close()
		t.Fatal("shared journal directory accepted")
	}
	if _, err := os.Stat(filepath.Join(path, "delivery.jsonl")); !os.IsNotExist(err) {
		t.Fatal("refused directory gained a journal")
	}
}
