// Атомарная публикация комплекта: ссылки, блокировка, отказы.
// Порт PublishTests из brood/dev/build/tests/test_artifacts.py.
package build

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// publishCase обрамление с планом и комплектом.
type publishCase struct {
	root   string
	plan   map[string]any
	bundle map[string]any
	output string
}

func newPublishCase(t *testing.T) *publishCase {
	t.Helper()
	root := t.TempDir()
	plan := artifactPlan(t)
	bundle, err := Render(plan, artifactImage(plan, "a"), nil)
	wantNoError(t, err)
	return &publishCase{root: root, plan: plan, bundle: bundle,
		output: filepath.Join(root, ".brood", "build", "development")}
}

func (c *publishCase) publish(t *testing.T, bundle map[string]any) string {
	t.Helper()
	output, err := Publish(c.root, "development", bundle)
	wantNoError(t, err)
	return output
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeOneJSON(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func TestCompleteImmutableRunsAndAtomicStablePointer(t *testing.T) {
	c := newPublishCase(t)
	stable := c.publish(t, c.bundle)
	if stable != c.output {
		t.Fatalf("output: %s", stable)
	}
	info, err := os.Lstat(stable)
	wantNoError(t, err)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("output must be a symlink")
	}
	link, err := os.Readlink(stable)
	wantNoError(t, err)
	first := filepath.Join(c.root, ".brood", "build", ".runs", "development",
		filepath.Base(link))
	if filepath.Dir(first) != filepath.Join(c.root, ".brood", "build", ".runs", "development") {
		t.Fatalf("run directory: %s", first)
	}
	entries, err := os.ReadDir(first)
	wantNoError(t, err)
	if len(entries) != len(c.bundle) {
		t.Fatalf("run files: %d", len(entries))
	}
	for name, value := range c.bundle {
		if !reflect.DeepEqual(readJSONFile(t, filepath.Join(stable, name)), value) {
			t.Fatalf("file %s mismatch", name)
		}
	}
	// Новый комплект меняет ссылку, прежний run остаётся нетронутым.
	updated, err := Render(c.plan, artifactImage(c.plan, "b"), nil)
	wantNoError(t, err)
	if got := c.publish(t, updated); got != stable {
		t.Fatal("output path must stay stable")
	}
	newLink, err := os.Readlink(stable)
	wantNoError(t, err)
	if newLink == link {
		t.Fatal("pointer must move to the new run")
	}
	if !reflect.DeepEqual(readJSONFile(t, filepath.Join(first, "build.json")), c.bundle["build.json"]) {
		t.Fatal("previous run must stay immutable")
	}
}

func TestWriteFailuresPreservePreviousSuccess(t *testing.T) {
	c := newPublishCase(t)
	c.publish(t, c.bundle)
	link, err := os.Readlink(c.output)
	wantNoError(t, err)
	// Имитация отказа на запись: каталог только для чтения не годится —
	// рушится и уборка; вместо этого публикуем неразрешимый комплект
	// (объект вместо строки в ресурсах даёт отказ кодирования до записи).
	brokenBundle := map[string]any{}
	for name, value := range c.bundle {
		brokenBundle[name] = value
	}
	updated, err := Render(c.plan, artifactImage(c.plan, "b"), nil)
	wantNoError(t, err)
	updated["build.json"] = map[string]any{"unexpected": true}
	_, err = Publish(c.root, "development", updated)
	wantError(t, err, "bundle")
	newLink, err := os.Readlink(c.output)
	wantNoError(t, err)
	if newLink != link {
		t.Fatal("previous pointer must be preserved on refusal")
	}
	if !reflect.DeepEqual(readJSONFile(t, filepath.Join(c.output, "build.json")), c.bundle["build.json"]) {
		t.Fatal("previous run must stay intact")
	}
	_ = brokenBundle
}

func TestForeignFilesDirectoriesLinksAndTamperedMarkerAreRefused(t *testing.T) {
	c := newPublishCase(t)
	if err := os.MkdirAll(filepath.Dir(c.output), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"file", "directory", "link"} {
		switch kind {
		case "file":
			wantNoError(t, os.WriteFile(c.output, []byte("mine"), 0o644))
		case "directory":
			wantNoError(t, os.Mkdir(c.output, 0o755))
		case "link":
			wantNoError(t, os.Symlink(filepath.Join(c.root, "missing"), c.output))
		}
		_, err := Publish(c.root, "development", c.bundle)
		if err == nil || !(strings.Contains(err.Error(), "foreign") || strings.Contains(err.Error(), "owned")) {
			t.Fatalf("kind %s: expected foreign/owned refusal, got %v", kind, err)
		}
		if kind == "directory" {
			wantNoError(t, os.Remove(c.output))
		} else {
			wantNoError(t, os.Remove(c.output))
		}
	}
	c.publish(t, c.bundle)
	marker := filepath.Join(c.output, "build.json")
	wantNoError(t, os.WriteFile(marker, []byte("{}"), 0o644))
	pointer, err := os.Readlink(c.output)
	wantNoError(t, err)
	_, err = Publish(c.root, "development", c.bundle)
	if err == nil || !(strings.Contains(err.Error(), "foreign") || strings.Contains(err.Error(), "owned")) {
		t.Fatalf("tampered marker: %v", err)
	}
	newPointer, err := os.Readlink(c.output)
	wantNoError(t, err)
	if newPointer != pointer {
		t.Fatal("pointer must not move for a tampered run")
	}
	data, err := os.ReadFile(marker)
	wantNoError(t, err)
	if string(data) != "{}" {
		t.Fatal("tampered marker must stay untouched")
	}
}

func TestSymlinkParentsAndTargetTraversalAreRefused(t *testing.T) {
	c := newPublishCase(t)
	for _, target := range []string{"", ".", "..", "../escape", "a/b", "/tmp/escape",
		"UPPER", strings.Repeat("a", 64)} {
		if _, err := Publish(c.root, target, c.bundle); err == nil {
			t.Errorf("target %q must be refused", target)
		}
	}
	for _, relative := range []string{".brood", ".brood/build", ".brood/build/.runs",
		".brood/build/.runs/development"} {
		directory := t.TempDir()
		link := filepath.Join(directory, relative)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		elsewhere := filepath.Join(directory, "elsewhere")
		if err := os.Mkdir(elsewhere, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, link); err != nil {
			t.Fatal(err)
		}
		_, err := Publish(directory, "development", c.bundle)
		if err == nil || !(strings.Contains(err.Error(), "symlink") || strings.Contains(err.Error(), "directory")) {
			t.Fatalf("parent %s: %v", relative, err)
		}
		entries, err := os.ReadDir(elsewhere)
		wantNoError(t, err)
		if len(entries) != 0 {
			t.Fatalf("elsewhere must stay empty, got %d entries", len(entries))
		}
	}
	alias := filepath.Join(c.root, "alias")
	if err := os.Symlink(c.root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(alias, "development", c.bundle); err == nil {
		t.Fatal("symlinked project root must be refused")
	}
}

func TestSameTargetPublicationsAreSerialized(t *testing.T) {
	c := newPublishCase(t)
	const workers = 8
	outputs := make([]string, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			output, err := Publish(c.root, "development", c.bundle)
			if err != nil {
				t.Errorf("publish: %v", err)
				return
			}
			outputs[slot] = output
		}(index)
	}
	group.Wait()
	for _, output := range outputs {
		if output != c.output {
			t.Fatalf("output: %s", output)
		}
	}
	runs := filepath.Join(c.root, ".brood", "build", ".runs", "development")
	entries, err := os.ReadDir(runs)
	wantNoError(t, err)
	if len(entries) != workers {
		t.Fatalf("expected %d runs, got %d", workers, len(entries))
	}
	for _, run := range entries {
		files, err := os.ReadDir(filepath.Join(runs, run.Name()))
		wantNoError(t, err)
		if len(files) != len(c.bundle) {
			t.Fatalf("run %s holds %d files", run.Name(), len(files))
		}
	}
	if !reflect.DeepEqual(readJSONFile(t, filepath.Join(c.output, "build.json")), c.bundle["build.json"]) {
		t.Fatal("final pointer must serve a valid bundle")
	}
}

func TestInvalidBundleNeverReplacesSuccess(t *testing.T) {
	c := newPublishCase(t)
	c.publish(t, c.bundle)
	link, err := os.Readlink(c.output)
	wantNoError(t, err)
	for _, bundle := range []map[string]any{
		{},
		func() map[string]any {
			changed := map[string]any{}
			for name, value := range c.bundle {
				changed[name] = value
			}
			changed["../escape"] = map[string]any{}
			return changed
		}(),
		func() map[string]any {
			changed := map[string]any{}
			for name, value := range c.bundle {
				changed[name] = value
			}
			changed["build.json"] = map[string]any{}
			return changed
		}(),
	} {
		if _, err := Publish(c.root, "development", bundle); err == nil {
			t.Errorf("bundle with keys %v must be refused", sortedKeys(bundle))
		}
		newLink, err := os.Readlink(c.output)
		wantNoError(t, err)
		if newLink != link {
			t.Fatal("invalid bundle must not move the pointer")
		}
	}
}
