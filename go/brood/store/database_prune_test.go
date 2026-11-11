package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// FEATURE: Deployment snapshots on the data volume are retained, not accumulated (#247).
// SPEC: Keep the snapshot of this deployment and the keep-1 newest other
// snapshots; unlink only regular files named .brood-snapshot-<32 hex>.db; never
// follow symlinks, never touch directories, the database or SQLite side files;
// refuse before any removal when the current snapshot is not a regular file.
// IMPLEMENTATION: PruneSnapshots over one directory listing and unlink(2).
// STATUS: DONE

func snapshotName(prefix string) string {
	return ".brood-snapshot-" + strings.Repeat(prefix, 32/len(prefix)) + ".db"
}

func writeAged(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte("synthetic-snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	moment := time.Now().Add(-age)
	if err := os.Chtimes(path, moment, moment); err != nil {
		t.Fatal(err)
	}
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestPruneSnapshotsKeepsCurrentAndNewestAndOnlyRemovesOwnFiles(t *testing.T) {
	dir := t.TempDir()
	// current is the oldest on purpose: it is kept by identity, not by age.
	current := filepath.Join(dir, snapshotName("a"))
	writeAged(t, current, 10*time.Hour)
	for prefix, age := range map[string]time.Duration{"1": 1 * time.Hour, "2": 2 * time.Hour, "3": 3 * time.Hour, "4": 4 * time.Hour} {
		writeAged(t, filepath.Join(dir, snapshotName(prefix)), age)
	}
	outside := filepath.Join(t.TempDir(), "outside.db")
	writeAged(t, outside, 20*time.Hour)
	foreign := []string{"brood.db", "brood.db-wal", snapshotName("5") + "-journal", ".brood-snapshot-UPPER.db",
		".brood-snapshot-" + strings.Repeat("b", 31) + ".db"}
	for _, name := range foreign {
		writeAged(t, filepath.Join(dir, name), 30*time.Hour)
	}
	link := filepath.Join(dir, snapshotName("6"))
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, snapshotName("7")), 0700); err != nil {
		t.Fatal(err)
	}

	result, err := PruneSnapshots(context.Background(), current, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Current != filepath.Base(current) || result.Kept != 3 || result.Removed != 2 {
		t.Fatalf("result = %+v", result)
	}
	want := append([]string{snapshotName("1"), snapshotName("2"), snapshotName("6"), snapshotName("7"),
		filepath.Base(current)}, foreign...)
	sort.Strings(want)
	if got := listNames(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("left %v, want %v", got, want)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("symlink target was touched")
	}
}

func TestPruneSnapshotsWithKeepOneLeavesOnlyCurrent(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, snapshotName("c"))
	writeAged(t, current, 2*time.Hour)
	writeAged(t, filepath.Join(dir, snapshotName("d")), time.Minute)
	result, err := PruneSnapshots(context.Background(), current, 1)
	if err != nil || result.Kept != 1 || result.Removed != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := listNames(t, dir); len(got) != 1 || got[0] != filepath.Base(current) {
		t.Fatalf("left %v", got)
	}
}

func TestPruneSnapshotsRefusesBeforeAnyRemoval(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, snapshotName("e"))
	writeAged(t, old, time.Hour)
	target := filepath.Join(t.TempDir(), snapshotName("f"))
	writeAged(t, target, time.Hour)
	link := filepath.Join(dir, snapshotName("9"))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(dir, snapshotName("8"))
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		current string
		keep    int
	}{
		"keep zero":     {filepath.Join(dir, snapshotName("e")), 0},
		"relative":      {snapshotName("e"), 1},
		"unclean":       {dir + "/./" + snapshotName("e"), 1},
		"foreign name":  {filepath.Join(dir, "brood.db"), 1},
		"missing":       {filepath.Join(dir, snapshotName("0")), 1},
		"symlink":       {link, 1},
		"directory":     {directory, 1},
		"empty":         {"", 1},
		"short hex":     {filepath.Join(dir, ".brood-snapshot-"+strings.Repeat("e", 31)+".db"), 1},
		"database path": {filepath.Join(dir, snapshotName("e")) + "/../brood.db", 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := PruneSnapshots(context.Background(), c.current, c.keep); err == nil {
				t.Fatal("prune accepted an unsafe current snapshot")
			}
			if _, err := os.Stat(old); err != nil {
				t.Fatal("refused prune removed a snapshot")
			}
		})
	}
}

func TestPruneSnapshotsHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, snapshotName("a"))
	writeAged(t, current, time.Hour)
	old := filepath.Join(dir, snapshotName("b"))
	writeAged(t, old, 2*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PruneSnapshots(ctx, current, 1); err == nil {
		t.Fatal("cancelled prune succeeded")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("cancelled prune removed a snapshot")
	}
}
