package build

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotsCanonicalizePackedObjectRepresentations(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "fetch.unpackLimit")
	t.Setenv("GIT_CONFIG_VALUE_0", "1")
	source := newGitFixture(t)
	for i := 0; i < 160; i++ {
		source.write(fmt.Sprintf("files/%04d", i), strings.Repeat("shared object content", 20)+fmt.Sprint(i))
	}
	source.commit()
	var results []map[string][32]byte
	for _, level := range []string{"0", "9"} {
		cached := filepath.Join(source.root, "cache-"+level)
		_, err := Snapshot(source.repo, source.sha, cached, true)
		wantNoError(t, err)
		_, err = Git(cached, "-c", "pack.compression="+level, "repack", "-a", "-d", "-F", "--window=0", "--threads=1")
		wantNoError(t, err)
		target := filepath.Join(source.root, "snapshot-"+level)
		_, err = Snapshot(cached, source.sha, target, true)
		wantNoError(t, err)
		contents := map[string][32]byte{}
		err = filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(target, path)
			if err != nil {
				return err
			}
			contents[relative] = sha256.Sum256(data)
			return nil
		})
		wantNoError(t, err)
		results = append(results, contents)
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatal("same commit has different context bytes due to Git pack representation")
	}
}
