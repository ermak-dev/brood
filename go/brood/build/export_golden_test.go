// Интеграционная проверка: Load и Export дают тот же build-plan.json,
// что и эталонная Python-реализация на том же commit (golden parity).
package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportGoldenBuildPlanJSON(t *testing.T) {
	f := newProjectFixture(t)
	loaded, err := LoadProject(f.repo, "", "", "localhost:5002", "build-test")
	if err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(f.root, "export")
	if _, err := Export(loaded, exported, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(exported, "build-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	sha := f.sha
	repository := "https://example.org/project.git"
	// Эталон построен json.dumps(sort_keys=True, indent=2) из Python на
	// том же фикстурном репозитории; значения подставлены по commit.
	expected := strings.Join([]string{
		`{`,
		`  "apiVersion": "brood.dev/v1alpha1",`,
		`  "build": {`,
		`    "arguments": {},`,
		`    "dockerfile": "project/environment/Dockerfile",`,
		`    "platform": "linux/amd64"`,
		`  },`,
		`  "environment": "python",`,
		`  "pins": {},`,
		`  "pinsFile": null,`,
		`  "project": "example",`,
		`  "sources": {`,
		`    "project": {`,
		`      "destination": "project",`,
		`      "gitMetadata": false,`,
		`      "repository": "` + repository + `",`,
		`      "revision": "` + sha + `",`,
		`      "type": "project"`,
		`    }`,
		`  }`,
		`}`,
		``,
	}, "\n")
	if string(data) != expected {
		t.Fatalf("build-plan.json mismatch:\ngot:\n%s\nwant:\n%s", data, expected)
	}
}
