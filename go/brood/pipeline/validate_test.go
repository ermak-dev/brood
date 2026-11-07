// Табличные проверки строгой валидации документа конвейера.
package pipeline

import (
	"os"
	"strings"
	"testing"
)

// validDocument читает эталонный пример из testdata.
func validDocument(t *testing.T) *Document {
	t.Helper()
	document, err := ParseFile("testdata/pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// BUG: graph validation accepts nodes the runner cannot execute and warns on human conditions.
// HYPOTHESIS: graph checks omit node references and execution constraints, and warnings are stale.
// SOLUTION: проверить ссылки и ограничения узлов в обеих формах валидации.
// RESULT: FIXED — неисполняемые графы отвергаются.
// STATUS: FIXED
func TestExecutionGraphConstraints(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Document)
		reason string
	}{
		{"missing branch", func(d *Document) { d.Nodes["prepare"] = Node{Type: "parallel", Branches: []string{"missing"}} }, "missing"},
		{"duplicate outcomes", func(d *Document) {
			n := d.Nodes["review"]
			n.Options = append(n.Options, n.Options[0])
			d.Nodes["review"] = n
		}, "outcome"},
		{"extra exit", func(d *Document) { d.Nodes["build"] = Node{Type: "exit"} }, "exit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDocument(t)
			tc.mutate(d)
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("Validate: expected %s error, got %v", tc.reason, err)
			}
			diags, err := d.Diagnostics()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, diag := range diags {
				if diag.Severity == ERROR && strings.Contains(diag.Message, tc.reason) {
					found = true
				}
			}
			if !found {
				t.Errorf("Diagnostics: expected %s error, got %+v", tc.reason, diags)
			}
		})
	}
}

func TestExampleDocumentIsValid(t *testing.T) {
	document := validDocument(t)
	if document.Name != "demo-pipeline" || document.Start != "prepare" || document.Exit != "done" {
		t.Fatalf("wrong header: %+v", document)
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("example must validate: %v", err)
	}
	diagnostics, err := document.Diagnostics()
	if err != nil {
		t.Fatalf("example diagnostics failed: %v", err)
	}
	for _, d := range diagnostics {
		if d.Severity == ERROR {
			t.Fatalf("example must not produce errors: %+v", d)
		}
	}
}

func TestParseStrictness(t *testing.T) {
	base := "testdata/pipeline.yaml"
	cases := []struct {
		name  string
		mut   func(t *testing.T) string
		field string
	}{
		{"unknown root field", func(t *testing.T) string {
			return readAndReplace(t, base, "start: prepare", "start: prepare\nunknownRoot: 1")
		}, "unknownRoot"},
		{"wrong apiVersion", func(t *testing.T) string {
			return readAndReplace(t, base, "apiVersion: brood.dev/v1alpha1", "apiVersion: example.com/v1")
		}, "apiVersion"},
		{"anchors forbidden", func(t *testing.T) string {
			return readAndReplace(t, base, "name: demo-pipeline", "name: &anchor demo-pipeline")
		}, "anchors"},
		{"duplicate mapping key", func(t *testing.T) string {
			return readAndPatch(t, base, "start: prepare\n", "start: prepare\nstart: build\n")
		}, "duplicate"},
		{"unknown node type", func(t *testing.T) string {
			return readAndReplace(t, base, "type: human", "type: robot")
		}, "type"},
		{"invalid name", func(t *testing.T) string {
			return readAndReplace(t, base, "name: demo-pipeline", "name: Demo Pipeline")
		}, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.mut(t)
			_, err := Parse(text, base)
			if err == nil {
				t.Fatalf("must be rejected")
			}
			if !strings.Contains(err.Error(), tc.field) && !strings.Contains(err.Error(), "line") {
				t.Fatalf("error %q must name the reason", err.Error())
			}
		})
	}
}

func TestValidateRejectsBrokenGraphs(t *testing.T) {
	base := "testdata/pipeline.yaml"
	cases := []struct {
		name   string
		mut    func(t *testing.T) string
		reason string
	}{
		{"unknown start", func(t *testing.T) string {
			return readAndReplace(t, base, "start: prepare", "start: missing")
		}, "start"},
		{"unknown exit", func(t *testing.T) string {
			return readAndReplace(t, base, "exit: done", "exit: missing")
		}, "exit"},
		{"edge endpoint missing", func(t *testing.T) string {
			return readAndReplace(t, base, "{from: land, to: done}", "{from: land, to: missing}")
		}, "missing"},
		{"exit with outgoing edge", func(t *testing.T) string {
			return readAndPatch(t, base, "  - {from: land, to: done}\n", "  - {from: land, to: done}\n  - {from: done, to: prepare}\n")
		}, "exit"},
		{"start with incoming edge", func(t *testing.T) string {
			// Вход в start из существующего узла: не из exit, чтобы
			// отказ указывал именно на вход в start.
			return readAndPatch(t, base, "  - {from: land, to: done}\n", "  - {from: land, to: done}\n  - {from: land, to: prepare}\n")
		}, "start"},
		{"unreachable node", func(t *testing.T) string {
			return readAndReplace(t, base, "{from: prepare, to: review}", "{from: review, to: build}")
		}, "reachable"},
		{"exit unreachable", func(t *testing.T) string {
			// Убираем оба пути в done: из check в exit больше не попасть.
			text := readAndPatch(t, base, "  - {from: land, to: done}\n", "")
			return patchText(t, text, "  - {from: review, to: done, condition: \"outcome=fail\"}\n", "")
		}, "not reachable"},
		{"goal gate is start", func(t *testing.T) string {
			return readAndReplace(t, base, "goalGates: [land]", "goalGates: [prepare]")
		}, "goal"},
		{"goal gate missing", func(t *testing.T) string {
			return readAndReplace(t, base, "goalGates: [land]", "goalGates: [missing]")
		}, "goal"},
		{"goal gates absent", func(t *testing.T) string {
			return readAndPatch(t, base, "goalGates: [land]\n", "")
		}, "goalGates"},
		{"bad condition key", func(t *testing.T) string {
			return readAndReplace(t, base, "outcome=success", "result=success")
		}, "condition"},
		{"bad condition operator", func(t *testing.T) string {
			return readAndReplace(t, base, "outcome=success", "outcome>=success")
		}, "condition"},
		{"empty condition value", func(t *testing.T) string {
			return readAndReplace(t, base, "outcome=success", "outcome=")
		}, "condition"},
		{"negative maxRetries", func(t *testing.T) string {
			return readAndReplace(t, base, "maxRetries: 1", "maxRetries: -1")
		}, "maxRetries"},
		{"human node without options", func(t *testing.T) string {
			// Дополнительный достижимый human-узел без options.
			text := readAndPatch(t, base,
				"  - {from: land, to: done}\n",
				"  - {from: land, to: done}\n  - {from: prepare, to: naked}\n  - {from: naked, to: done}\n")
			return patchText(t, text,
				"  done:\n    type: exit\n",
				"  naked:\n    type: human\n    question: q\n  done:\n    type: exit\n")
		}, "options"},
		{"human option without outcome", func(t *testing.T) string {
			return readAndReplace(t, base, "outcome: fail", "# removed")
		}, "outcome"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.mut(t)
			document, err := Parse(text, base)
			if err == nil {
				err = document.Validate()
			}
			if err == nil {
				t.Fatalf("must be rejected")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error %q must mention %q", err.Error(), tc.reason)
			}
		})
	}
}

func TestDiagnosticsWarns(t *testing.T) {
	document := validDocument(t)
	diagnostics, err := document.Diagnostics()
	if err != nil {
		t.Fatal(err)
	}
	// Условия человеческого узла допустимы; вес вместе с условием предупреждает.
	foundHuman, foundWeight := false, false
	for _, d := range diagnostics {
		if d.Severity == ERROR {
			t.Fatalf("valid document produced an error: %+v", d)
		}
		if d.Severity == WARNING {
			if strings.Contains(d.Message, "human") {
				foundHuman = true
			}
			if strings.Contains(d.Message, "weight") {
				foundWeight = true
			}
		}
	}
	if foundHuman || !foundWeight {
		t.Fatalf("unexpected warnings: human=%v weight=%v in %+v", foundHuman, foundWeight, diagnostics)
	}
	// Селектор стиля, не совпавший ни с одним узлом, — предупреждение.
	document.Styles[0].Selectors = []string{"missing-node"}
	diagnostics, err = document.Diagnostics()
	if err != nil {
		t.Fatal(err)
	}
	foundSelector := false
	for _, d := range diagnostics {
		if d.Severity == WARNING && strings.Contains(d.Message, "missing-node") {
			foundSelector = true
		}
	}
	if !foundSelector {
		t.Fatalf("no warning for style selector matching nothing: %+v", diagnostics)
	}
}

// patchText заменяет фрагмент old на patch в тексте (пустой patch —
// удалить фрагмент).
func patchText(t *testing.T, text, old, patch string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	if patch == "" {
		return strings.Replace(text, old, "", 1)
	}
	return strings.Replace(text, old, patch, 1)
}

// readAndPatch читает фикстуру и применяет patchText.
func readAndPatch(t *testing.T, path, old, patch string) string {
	t.Helper()
	return patchText(t, readExample(t, path), old, patch)
}

func readAndReplace(t *testing.T, path, old, new string) string {
	t.Helper()
	text := readExample(t, path)
	result := strings.Replace(text, old, new, 1)
	if result == text && old != new {
		t.Fatalf("fixture does not contain %q", old)
	}
	return result
}

func readExample(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// FEATURE: Managed pool workflow actors.
// SPEC: Actor sources cannot conflict; pool is a DNS label and belongs only to actors.
// IMPLEMENTATION: Validate actor sources without resolving external pools.
// RESULT: IMPLEMENTED — legacy templates and managed pools are supported.
// STATUS: DONE
func TestManagedPoolParsing(t *testing.T) {
	for _, tc := range []struct {
		name, node string
		valid      bool
	}{
		{"pool", "type: actor, managedPool: external-pool", true},
		{"legacy", "type: actor, template: python", true},
		{"both", "type: actor, template: python, managedPool: pool", false},
		{"neither (legacy)", "type: actor", true},
		{"empty", "type: actor, managedPool: \"\"", false},
		{"malformed", "type: actor, managedPool: Bad_Pool", false},
		{"nonactor", "type: exit, managedPool: pool", false},
		{"nonactor empty", "type: exit, managedPool: \"\"", false},
		{"null", "type: actor, managedPool: null", false},
		{"numeric", "type: actor, managedPool: 123", false},
		{"empty template with pool", "type: actor, template: \"\", managedPool: pool", true},
		{"wrong type", "type: actor, managedPool: [pool]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "apiVersion: brood.dev/v1alpha1\nname: test\nstart: work\nexit: work\nnodes:\n  work: {" + tc.node + "}\nedges: []\ngoalGates: []\n"
			_, err := Parse(text, "test.yaml")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

// BUG: Style defaults and null execution fields are validated inconsistently.
// HYPOTHESIS: Source validation ignores styles and YAML null decodes as an empty string.
// SOLUTION: Validate effective style defaults and require string execution fields.
// RESULT: FIXED — explicit sources override defaults and null is rejected.
// STATUS: FIXED
func TestStyledActorSources(t *testing.T) {
	for _, tc := range []struct {
		name, node, selector string
		valid                bool
	}{
		{"name", "type: actor", "work", true},
		{"type", "type: actor", "type:actor", true},
		{"wildcard", "type: actor", "*", true},
		{"mismatch (legacy source-less actor)", "type: actor", "other", true},
		{"pool overrides style", "type: actor, managedPool: pool", "work", true},
		{"both explicit", "type: actor, template: python, managedPool: pool", "work", false},
		{"null template", "type: actor, template: null, managedPool: pool", "work", false},
		{"null pool", "type: actor, template: python, managedPool: null", "work", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "apiVersion: brood.dev/v1alpha1\nname: test\nstart: work\nexit: done\nnodes:\n  work: {" + tc.node + "}\n  done: {type: exit}\nedges: [{from: work, to: done}]\ngoalGates: [done]\nstyles:\n  - selectors: [\"" + tc.selector + "\"]\n    template: python\n"
			d, err := Parse(text, "test.yaml")
			if err == nil {
				err = d.Validate()
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestEffectiveTemplatePrecedence(t *testing.T) {
	d := &Document{Nodes: map[string]Node{"work": {Type: "actor"}}, Styles: []Style{
		{Selectors: []string{"*"}, Template: "first"},
		{Selectors: []string{"type:actor"}, Template: "last"},
		{Selectors: []string{"other"}, Template: "unmatched"},
	}}
	if got := d.EffectiveTemplate("work"); got != "last" {
		t.Fatalf("style default = %q", got)
	}
	d.Nodes["work"] = Node{Type: "actor", Template: "explicit"}
	if got := d.EffectiveTemplate("work"); got != "explicit" {
		t.Fatalf("explicit template = %q", got)
	}
	d.Nodes["work"] = Node{Type: "actor", ManagedPool: "pool"}
	if got := d.EffectiveTemplate("work"); got != "" {
		t.Fatalf("pool inherited template %q", got)
	}
}

// BUG: Legacy workflow snapshots without actor sources cannot be read.
// HYPOTHESIS: New source validation rejects formerly valid source-less actors.
// SOLUTION: Permit absent sources while rejecting explicit conflicts.
// RESULT: FIXED — legacy snapshots pass all validation paths.
// STATUS: FIXED
func TestLegacySnapshotWithoutActorSource(t *testing.T) {
	text := "apiVersion: brood.dev/v1alpha1\nname: legacy\nstart: work\nexit: done\nnodes:\n  work: {type: actor, prompt: go}\n  done: {type: exit}\nedges: [{from: work, to: done}]\ngoalGates: [done]\n"
	d, err := Parse(text, "legacy.yaml")
	if err != nil {
		t.Fatalf("parse legacy snapshot: %v", err)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("validate legacy snapshot: %v", err)
	}
	if _, err := d.Diagnostics(); err != nil {
		t.Fatalf("diagnose legacy snapshot: %v", err)
	}
}

// BUG: Legacy null templates are rejected when reading old snapshots.
// HYPOTHESIS: Source checking rejects YAML null before legacy decoding.
// SOLUTION: Preserve legacy decoding unless a managed pool is also specified.
// RESULT: FIXED — old null templates parse without allowing ambiguous sources.
// STATUS: FIXED
func TestLegacyNullTemplates(t *testing.T) {
	text := "apiVersion: brood.dev/v1alpha1\nname: legacy\nstart: work\nexit: done\nnodes:\n  work: {type: actor, template: null}\n  done: {type: exit}\nedges: [{from: work, to: done}]\ngoalGates: [done]\nstyles:\n  - selectors: [work]\n    template: null\n"
	d, err := Parse(text, "legacy.yaml")
	if err != nil {
		t.Fatalf("parse legacy null templates: %v", err)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPoolProgrammaticValidation(t *testing.T) {

	d := &Document{Name: "test", Start: "work", Exit: "done", Nodes: map[string]Node{"work": {Type: "actor"}, "done": {Type: "exit"}}, Edges: []Edge{{From: "work", To: "done"}}, GoalGates: []string{"done"}}

	d.Nodes["work"] = Node{Type: "actor", Template: "python", ManagedPool: "external"}
	if err := d.Validate(); err == nil {
		t.Fatal("Validate accepted conflicting actor sources")
	}

	if _, err := d.Diagnostics(); err == nil {
		t.Fatal("Diagnostics accepted conflicting actor sources")
	}

	d.Nodes["work"] = Node{Type: "actor", ManagedPool: "external"}

	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Diagnostics(); err != nil {
		t.Fatal(err)
	}

}
