package build

import (
	"path/filepath"
	"testing"
)

// FEATURE: provenance отличает рабочие файлы от точного commit-снимка.
// SPEC: HEAD — только базовая revision; изменённая рабочая копия помечена явно.
func TestWorkingSourceProvenanceDoesNotClaimCleanCommit(t *testing.T) {
	f := newProjectFixture(t)
	f.write("code.py", "version = 2\n")
	report, err := WorkingSnapshot(f.repo, filepath.Join(t.TempDir(), "source"), true)
	if err != nil {
		t.Fatal(err)
	}
	if report["workingTree"] != true || report["dirty"] != true {
		t.Fatalf("missing working-copy provenance: %+v", report)
	}
	project, err := LoadWorkingProject(f.repo, "", "dev")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := Render(project.Plan, artifactImage(project.Plan, "a"), map[string]any{"project": report})
	if err != nil {
		t.Fatal(err)
	}
	source := artifacts["build.json"].(map[string]any)["sources"].(map[string]any)["project"].(map[string]any)
	if source["workingTree"] != true || source["dirty"] != true {
		t.Fatalf("build metadata dropped working-copy identity: %+v", source)
	}
}
