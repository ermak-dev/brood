package build

// Каталог #290 задаёт только скелеты; условия ворот принадлежат политике проекта.
// Проверяем настоящий корневой документ, публичный loader и машину переходов.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"brood.dev/go/brood/pipeline"
)

func rootCatalog(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestRootCatalogSkeletons(t *testing.T) {
	doc := rootCatalog(t)
	workflows := doc["workflows"].(map[string]any)
	var names []string
	for name := range workflows {
		names = append(names, name)
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, []string{"develop", "docs", "fix"}) {
		t.Errorf("catalog names = %v, want develop, docs, fix", names)
	}
	for _, name := range []string{"develop", "fix", "docs"} {
		t.Run(name, func(t *testing.T) {
			graph, err := Workflow(doc, name, "Broodfile")
			if err != nil {
				t.Fatal(err)
			}
			if graph.Start != "implement" || graph.Exit != "done" || !reflect.DeepEqual(graph.GoalGates, []string{"done"}) {
				t.Fatalf("unexpected endpoints or goal gates: %+v", graph)
			}
			if len(graph.Nodes) != 4 || len(graph.Edges) != 3 || graph.Retry != nil {
				t.Fatalf("expected only implement, review, land, done without retries: %+v", graph)
			}
			if graph.Nodes["done"].Type != "exit" {
				t.Fatal("done is not the exit")
			}
			if roles := graph.Roles(); !reflect.DeepEqual(roles, map[string]bool{"build": true, "review": true}) {
				t.Fatalf("roles = %v, want build and review", roles)
			}
			stages := []struct{ id, role, class, next string }{
				{"implement", "build", "work", "review"},
				{"review", "review", "review", "land"},
				{"land", "build", "landing", "done"},
			}
			begin, err := pipeline.Begin(graph)
			if err != nil || begin.Next != "implement" || begin.Execution != "running" {
				t.Fatalf("begin = %+v, %v", begin, err)
			}
			for _, stage := range stages {
				node := graph.Nodes[stage.id]
				if node.Type != "actor" || node.Role != stage.role || !reflect.DeepEqual(node.ActionClasses, []string{stage.class}) || strings.TrimSpace(node.Prompt) == "" {
					t.Fatalf("%s: unexpected actor contract: %+v", stage.id, node)
				}
				var outgoing []pipeline.Edge
				for _, edge := range graph.Edges {
					if edge.From == stage.id {
						outgoing = append(outgoing, edge)
					}
				}
				if len(outgoing) != 1 || outgoing[0].To != stage.next || outgoing[0].Condition != "outcome=success" {
					t.Fatalf("%s: success-only routing = %+v", stage.id, outgoing)
				}
				want := pipeline.Transition{Next: stage.next, NextState: "pending", Execution: "running"}
				if stage.next == "done" {
					want.NextState, want.Execution = "completed", "succeeded"
				}
				got, err := pipeline.Advance(graph, pipeline.Event{Node: stage.id, State: "completed", Outcome: "success"})
				if err != nil || got != want {
					t.Fatalf("%s success = %+v, %v; want %+v", stage.id, got, err, want)
				}
				got, err = pipeline.Advance(graph, pipeline.Event{Node: stage.id, State: "completed", Outcome: "fail"})
				if err != nil || got != (pipeline.Transition{Execution: "failed", Terminal: true}) {
					t.Fatalf("%s failure must terminate without successor: %+v, %v", stage.id, got, err)
				}
			}
		})
	}
}

func TestRootCatalogPublicProjectLoader(t *testing.T) {
	doc := rootCatalog(t)
	want, err := WorkflowSnapshots(doc, "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"", "development", "coding"} {
		t.Run("target="+target, func(t *testing.T) {
			project, err := LoadWorkingProject("../../..", target, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(project.Document, doc) {
				t.Fatal("public project loader changed the catalog document")
			}
			snapshots, err := WorkflowSnapshots(project.Document, "Broodfile")
			if err != nil || !reflect.DeepEqual(snapshots, want) || len(snapshots) != 3 {
				t.Fatalf("loaded snapshots = %v, %v", snapshots, err)
			}
			for name, snapshot := range snapshots {
				restored, err := pipeline.Parse(snapshot, name)
				if err != nil {
					t.Fatal(err)
				}
				original, err := Workflow(doc, name, "Broodfile")
				if err != nil || !reflect.DeepEqual(restored, original) {
					t.Fatalf("%s: project publication lost graph data: %v", name, err)
				}
			}
		})
	}
}

func TestRootCatalogPreservesBuildDeclaration(t *testing.T) {
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	prefix, _, ok := strings.Cut(string(data), "\nworkflows:\n")
	if !ok {
		t.Fatal("missing workflows block")
	}
	// Смена каталога не должна менять поля сборки, ресурсов и сети вне workflows.
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(prefix+"\n"))); got != "64be0f174cd9634cd548b104191408be58ff6d20cb548195b65c8eb8ff91bb92" {
		t.Fatalf("non-workflow declaration changed: %s", got)
	}
}

func TestRootCatalogPromptContracts(t *testing.T) {
	doc := rootCatalog(t)
	for _, name := range []string{"develop", "fix", "docs"} {
		t.Run(name, func(t *testing.T) {
			graph, err := Workflow(doc, name, "Broodfile")
			if err != nil {
				t.Fatal(err)
			}
			contracts := map[string][]string{
				"implement": {"CLAUDE.md", "duplicates", "issue", "branch", "one change per layer", "failing test", "before", "brood dev test", "ruff check", "go build ./...", "go test ./...", "brood dev web test", "browser", "Go API", "ask_operator", "architecture", "blockers"},
				"review":    {"independent", "fresh session", "different actor", "exact", "diff", "tests", "unresolved", "fail"},
				"land":      {"current", "permissions", "external", "merge request", "main", "CI", "merged", "commit", "proof", "fail", "deploy"},
			}
			for stage, required := range contracts {
				prompt := graph.Nodes[stage].Prompt
				for _, phrase := range required {
					if !strings.Contains(prompt, phrase) {
						t.Errorf("%s prompt missing %q", stage, phrase)
					}
				}
				if !strings.Contains(prompt, "Do not fabricate") {
					t.Errorf("%s must prohibit fabricated evidence or effects", stage)
				}
				if strings.Contains(prompt, "selected externally") {
					t.Errorf("%s retains external workflow selection dead end", stage)
				}
			}
			for _, phrase := range map[string][]string{
				"develop": {"scoped feature"},
				"fix":     {"reproduce", "regression test", "root cause"},
				"docs":    {"documentation-only", "examples", "links", "Do not change runtime"},
			}[name] {
				if !strings.Contains(graph.Nodes["implement"].Prompt, phrase) {
					t.Errorf("%s work prompt missing scope %q", name, phrase)
				}
			}
		})
	}
}
