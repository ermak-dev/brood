package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Bounded, credential-free project configuration survives local builds.
// SPEC: Capacity uses API decimal strings, optional fields preserve presence,
// repository resolution uses only the exact selected checkout and explicit wins.
// IMPLEMENTATION: Closed schema and semantic validation feed independent plan/build metadata.
// RESULT: IMPLEMENTED — declaration boundaries and exact-root resolution pass.
// STATUS: DONE
func TestDeclarationContractSchemaAndPlan(t *testing.T) {
	capacity := map[string]any{"worker_selector": map[string]any{"pool": "one"}, "max_swarms": "0", "max_actors": nil, "max_cpu_millicores": "9007199254740993", "max_memory_bytes": "9223372036854775807"}
	doc := minimal(t)
	doc["project"] = map[string]any{"repository": "https://Git.Example.com/team/repo.git", "description": "", "capacity": capacity}
	plan, err := MakePlan(doc, nil, "", "localhost:5000/team", "destination", "Broodfile")
	wantNoError(t, err)
	want := map[string]any{"name": "example", "repository": "https://git.example.com/team/repo.git", "description": "", "capacity": capacity}
	if !reflect.DeepEqual(plan["projectConfig"], want) {
		t.Fatalf("configuration = %v", plan["projectConfig"])
	}
	artifacts := mustRender(t, plan, artifactImage(plan, "a"))
	metadata := artifacts["build.json"].(map[string]any)
	if !reflect.DeepEqual(metadata["projectConfig"], want) {
		t.Fatalf("rendered configuration = %v", metadata["projectConfig"])
	}
	data, err := json.Marshal(metadata)
	wantNoError(t, err)
	if !strings.Contains(string(data), `"max_cpu_millicores":"9007199254740993"`) {
		t.Fatal("large capacity lost precision")
	}
	metadata["projectConfig"].(map[string]any)["name"] = "changed"
	if !reflect.DeepEqual(plan["projectConfig"], want) {
		t.Fatal("render aliases plan")
	}
}

func TestDeclarationRejectsInvalidFields(t *testing.T) {
	cases := []map[string]any{
		{"token": "DO_NOT_ECHO"}, {"name": "other"}, {"repository": true}, {"repository": nil}, {"description": false}, {"description": strings.Repeat("x", 4097)}, {"description": "bad\x00text"},
		{"capacity": nil}, {"capacity": map[string]any{}}, {"capacity": map[string]any{"worker_selector": map[string]any{}}},
		{"capacity": map[string]any{"worker_selector": map[string]any{"pool": true}}},
	}
	for _, value := range []any{true, 0, int64(1), 1.0, "-1", "01", "+1", "1.0", "9223372036854775808", ""} {
		cases = append(cases, map[string]any{"capacity": map[string]any{"worker_selector": map[string]any{"pool": "one"}, "max_actors": value}})
	}
	for _, project := range cases {
		doc := minimal(t)
		doc["project"] = project
		_, err := MakePlan(doc, nil, "", "localhost:5000/team", "development", "Broodfile")
		if err == nil || strings.Contains(err.Error(), "DO_NOT_ECHO") {
			t.Fatalf("invalid declaration accepted or exposed: %v", err)
		}
	}
	for _, description := range []string{"", strings.Repeat("x", 4096)} {
		doc := minimal(t)
		doc["project"] = map[string]any{"description": description}
		_, err := MakePlan(doc, nil, "", "localhost:5000/team", "development", "Broodfile")
		wantNoError(t, err)
	}
}

func TestDeclarationResolutionPresenceAndExactRoot(t *testing.T) {
	f := newProjectFixture(t)
	f.git("remote", "set-url", "origin", "https://user:DO_NOT_ECHO@git.example.com/repo")
	_, err := LoadWorkingProject(f.repo, "", "")
	if err == nil || strings.Contains(err.Error(), "DO_NOT_ECHO") {
		t.Fatalf("unsafe origin accepted or exposed: %v", err)
	}
	file := filepath.Join(f.repo, "Broodfile")
	original, err := os.ReadFile(file)
	wantNoError(t, err)
	for _, repository := range []string{"", "https://git.example.com/explicit"} {
		wantNoError(t, os.WriteFile(file, append(append([]byte{}, original...), []byte("project:\n  repository: '"+repository+"'\n")...), 0600))
		p, err := LoadWorkingProject(f.repo, "", "destination")
		wantNoError(t, err)
		if !resolvedRepository(t, p) || p.Plan["projectConfig"].(map[string]any)["repository"] != repository {
			t.Fatal("explicit repository not preserved")
		}
	}
	f.git("remote", "remove", "origin")
	wantNoError(t, os.WriteFile(file, original, 0600))
	p, err := LoadWorkingProject(f.repo, "", "")
	wantNoError(t, err)
	if resolvedRepository(t, p) || p.Plan["projectConfig"].(map[string]any)["repository"] != "" {
		t.Fatal("missing origin resolved")
	}
	nested := filepath.Join(f.repo, "nested")
	wantNoError(t, os.Mkdir(nested, 0700))
	wantNoError(t, os.WriteFile(filepath.Join(nested, "Broodfile"), original, 0600))
	f.git("remote", "add", "origin", "https://git.example.com/parent")
	p, err = LoadWorkingProject(nested, "", "")
	wantNoError(t, err)
	if resolvedRepository(t, p) || p.Plan["projectConfig"].(map[string]any)["repository"] != "" {
		t.Fatal("inherited unrelated ancestor origin")
	}
	p, err = LoadWorkingProject(f.repo, "", "")
	wantNoError(t, err)
	if !resolvedRepository(t, p) {
		t.Fatal("safe origin not marked resolved")
	}
}

func resolvedRepository(t *testing.T, p *Project) bool {
	t.Helper()
	field := reflect.ValueOf(p).Elem().FieldByName("RepositoryResolved")
	if !field.IsValid() || field.Kind() != reflect.Bool {
		t.Fatal("Project.RepositoryResolved bool is missing")
	}
	return field.Bool()
}
