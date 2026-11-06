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
