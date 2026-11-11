package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
)

// deploySnapshot matches the private snapshots that deployment helpers write
// next to the database: .brood-snapshot-<uuid4 hex>.db (existing_install.py).
var deploySnapshot = regexp.MustCompile(`^\.brood-snapshot-[0-9a-f]{32}\.db$`)

// SnapshotRetention is the deployment-safe result of PruneSnapshots: the kept
// current snapshot name and counts, no other file names or contents.
type SnapshotRetention struct {
	Current string `json:"current"`
	Kept    int    `json:"kept"`
	Removed int    `json:"removed"`
}

// PruneSnapshots keeps the snapshot current and the keep-1 newest other
// deployment snapshots in its directory and unlinks the rest. Only regular
// files named like deployment snapshots are considered: symlinks, directories,
// the database and SQLite side files are never touched. current must be a
// clean absolute path of such a regular file before and after pruning, so
// the snapshot of the running deployment is never removed.
func PruneSnapshots(ctx context.Context, current string, keep int) (SnapshotRetention, error) {
	var result SnapshotRetention
	if ctx.Err() != nil {
		return result, errors.New("database operation cancelled or timed out")
	}
	if keep < 1 {
		return result, errors.New("snapshot retention must keep at least one snapshot")
	}
	if !filepath.IsAbs(current) || filepath.Clean(current) != current || !deploySnapshot.MatchString(filepath.Base(current)) {
		return result, errors.New("current snapshot must be a clean absolute .brood-snapshot-<hex>.db path")
	}
	if !regularFile(current) {
		return result, errors.New("current snapshot must exist as a regular file")
	}
	dir, name := filepath.Dir(current), filepath.Base(current)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result, errors.New("cannot list the snapshot directory")
	}
	type snapshot struct {
		name     string
		modified time.Time
	}
	var others []snapshot
	for _, entry := range entries {
		if entry.Name() == name || !deploySnapshot.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // удалён между листингом и stat: удалять нечего
		}
		others = append(others, snapshot{entry.Name(), info.ModTime()})
	}
	sort.Slice(others, func(i, j int) bool {
		if !others[i].modified.Equal(others[j].modified) {
			return others[i].modified.After(others[j].modified)
		}
		return others[i].name > others[j].name
	})
	result.Current, result.Kept = name, 1
	for i, other := range others {
		if ctx.Err() != nil {
			return result, errors.New("database operation cancelled or timed out")
		}
		if i < keep-1 {
			result.Kept++
			continue
		}
		// unlink(2) не идёт по симлинку и не удаляет каталог, в отличие от RemoveAll.
		err := syscall.Unlink(filepath.Join(dir, other.name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, errors.New("cannot remove an old snapshot")
		}
		result.Removed++
	}
	if !regularFile(current) {
		return result, errors.New("current snapshot disappeared during pruning")
	}
	return result, nil
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
