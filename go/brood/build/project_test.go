// Broodfile и контекст читаются из одного commit, не из рабочей копии.
// Порт brood/dev/build/tests/test_project.py.
package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projectFixture — репозиторий с фикстурой minimal.yaml, как ProjectTests.
type projectFixture struct {
	*gitFixture
}

func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()
	f := &projectFixture{newGitFixture(t)}
	data, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f.write("Broodfile", string(data))
	f.write("environment/Dockerfile", "FROM scratch\n")
	f.commit()
	return f
}

func (f *projectFixture) load(t *testing.T, target string) *Project {
	t.Helper()
	loaded, err := LoadProject(f.repo, "", target, "localhost:5002", "build-test")
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestReaderUsesSelectedCommitAndReportsExcludedChanges(t *testing.T) {
	f := newProjectFixture(t)
	f.write("Broodfile", "invalid working tree")
	loaded := f.load(t, "")
	if got := loaded.Plan["project"]; got != "example" {
		t.Fatalf("project: %v", got)
	}
	if loaded.Revision != f.sha {
		t.Fatalf("revision: %v", loaded.Revision)
	}
	if !loaded.Dirty {
		t.Fatal("dirty changes must be reported")
	}
	if loaded.Root != f.repo {
		t.Fatalf("root: %v", loaded.Root)
	}
}

func TestExportIsPortableAndContainsNoWorkingTreeOverlays(t *testing.T) {
	f := newProjectFixture(t)
	loaded := f.load(t, "")
	f.write("code.py", "dirty")
	exported := filepath.Join(f.root, "export")
	reports, err := Export(loaded, exported, nil)
	wantNoError(t, err)
	if got := f.read(t, exported, "project/code.py"); got != "version = 1\n" {
		t.Fatalf("project/code.py: %q", got)
	}
	if got := reports["project"].(map[string]any)["revision"]; got != f.sha {
		t.Fatalf("report revision: %v", got)
	}
	data, err := os.ReadFile(filepath.Join(exported, "build-plan.json"))
	wantNoError(t, err)
	plan := map[string]any{}
	wantNoError(t, json.Unmarshal(data, &plan))
	sources := plan["sources"].(map[string]any)
	if got := sources["project"].(map[string]any)["revision"]; got != f.sha {
		t.Fatalf("plan revision: %v", got)
	}
	if strings.Contains(string(data), f.repo) {
		t.Fatalf("build-plan.json leaks machine paths: %s", data)
	}
}

func TestRuntimeOnlyTargetFieldsDoNotChangeContextMetadata(t *testing.T) {
	f := newProjectFixture(t)
	document := minimal(t)
	coding := deepCopy(t, document["templates"].(map[string]any)["python"])
	codingMap := coding.(map[string]any)
	codingMap["actor"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"] = []any{"interactive"}
	document["templates"].(map[string]any)["coding"] = codingMap
	document["targets"].(map[string]any)["coding"] = map[string]any{"template": "coding"}
	f.write("Broodfile", marshal(t, document))
	f.commit()
	first, second := filepath.Join(f.root, "first"), filepath.Join(f.root, "second")
	_, err := Export(f.load(t, ""), first, nil)
	wantNoError(t, err)
	_, err = Export(f.load(t, "coding"), second, nil)
	wantNoError(t, err)
	left, err := os.ReadFile(filepath.Join(first, "build-plan.json"))
	wantNoError(t, err)
	right, err := os.ReadFile(filepath.Join(second, "build-plan.json"))
	wantNoError(t, err)
	if string(left) != string(right) {
		t.Fatalf("build-plan.json must not depend on runtime-only fields:\n%s\n%s", left, right)
	}
}

func TestExportDoesNotOverwriteExistingPath(t *testing.T) {
	f := newProjectFixture(t)
	exported := filepath.Join(f.root, "export")
	if err := os.Mkdir(exported, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exported, "mine"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Export(f.load(t, ""), exported, nil)
	wantError(t, err, "exists")
	if got := f.read(t, exported, "mine"); got != "keep" {
		t.Fatalf("mine: %q", got)
	}
}

func TestMissingDockerfileLeavesNoPartialContext(t *testing.T) {
	f := newProjectFixture(t)
	if err := os.Remove(filepath.Join(f.repo, "environment/Dockerfile")); err != nil {
		t.Fatal(err)
	}
	f.commit()
	exported := filepath.Join(f.root, "export")
	_, err := Export(f.load(t, ""), exported, nil)
	wantError(t, err, "Dockerfile")
	if _, err := os.Lstat(exported); err == nil {
		t.Fatal("partial context must not appear")
	}
}

func TestProjectWithoutOriginRendersArtifactsWithoutRepository(t *testing.T) {
	f := newProjectFixture(t)
	f.git("remote", "remove", "origin")
	selected := f.load(t, "")
	reports, err := Export(selected, filepath.Join(f.root, "export"), nil)
	wantNoError(t, err)
	digest := "sha256:" + strings.Repeat("a", 64)
	bundle, err := Render(selected.Plan, "localhost:5002/example/python@"+digest, reports)
	wantNoError(t, err)
	provenance := bundle["build.json"].(map[string]any)
	if _, ok := provenance["sources"].(map[string]any)["project"].(map[string]any)["repository"]; ok {
		t.Fatal("repository must be absent without origin")
	}
}

func TestPinsReadFromTheSameCommit(t *testing.T) {
	f := newProjectFixture(t)
	data, err := os.ReadFile(filepath.Join(f.repo, "Broodfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	text = strings.Replace(text, "name: example", "name: example\npinsFile: pins.json", 1)
	text = strings.Replace(text, "platform: linux/amd64",
		"platform: linux/amd64\n      argsFromPins: [VERSION]", 1)
	f.write("Broodfile", text)
	f.write("pins.json", `{"VERSION": "1"}`)
	f.commit()
	f.write("pins.json", `{"VERSION": "dirty"}`)
	loaded := f.load(t, "")
	arguments := loaded.Plan["build"].(map[string]any)["arguments"]
	if !strings.Contains(marshal(t, arguments), `"VERSION":"1"`) {
		t.Fatalf("arguments: %s", marshal(t, arguments))
	}
}

func TestPinsDuplicateJSONKeysAreRefused(t *testing.T) {
	f := newProjectFixture(t)
	data, err := os.ReadFile(filepath.Join(f.repo, "Broodfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(data), "name: example",
		"name: example\npinsFile: pins.json", 1)
	text = strings.Replace(text, "platform: linux/amd64",
		"platform: linux/amd64\n      argsFromPins: [VERSION]", 1)
	f.write("Broodfile", text)
	f.write("pins.json", `{"VERSION": "1", "VERSION": "2"}`)
	f.commit()
	_, err = LoadProject(f.repo, "", "", "localhost:5002", "build-test")
	wantError(t, err, "pins")
}
