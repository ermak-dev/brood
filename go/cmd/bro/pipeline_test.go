// Проверки команд bro pipeline: локальный файл, без сервера.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readTestFile читает файл фикстуры.
func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeTestFile пишет файл фикстуры с приватными правами.
func writeTestFile(t *testing.T, path string, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

// pipelineFile пишет копию эталонного примера во временный каталог.
func writePipeline(t *testing.T) string {
	t.Helper()
	return writePipelineText(t, validPipelineText(t))
}

func validPipelineText(t *testing.T) string {
	t.Helper()
	return string(readTestFile(t, filepath.Join("..", "..", "brood", "pipeline", "testdata", "pipeline.yaml")))
}

// writePipelineText пишет произвольный текст конвейера в файл.
func writePipelineText(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.yaml")
	writeTestFile(t, path, text)
	return path
}

// FEATURE: Select a pipeline workflow from a Broodfile.
// SPEC: All commands accept --workflow NAME and an optional Broodfile; unknown names fail.
// IMPLEMENTATION: Reuse the build parser and workflow validator for embedded graphs.
// RESULT: IMPLEMENTED — standalone and embedded pipelines share pipeline operations.
// STATUS: DONE
func TestPipelineBroodfileWorkflow(t *testing.T) {
	isolateState(t)
	path := filepath.Join("..", "..", "..", "examples", "Broodfile")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"validate", "--workflow", "inspect", path}, "INFO"},
		{[]string{"plan", "--workflow", "inspect", path}, "inspect -> review"},
		{[]string{"render", "--workflow", "inspect", path, "--dot"}, "inspect -> review"},
	} {
		code, out, diagnostic := invoke(t, "", append([]string{"pipeline"}, tc.args...)...)
		if code != 0 || !strings.Contains(out, tc.want) || diagnostic != "" {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, diagnostic)
		}
	}
	for _, sub := range []string{"validate", "plan", "render"} {
		args := []string{"pipeline", sub, "--workflow", "missing", path}
		if sub == "render" {
			args = append(args, "--dot")
		}
		code, _, diagnostic := invoke(t, "", args...)
		if code == 0 || !strings.Contains(diagnostic, "missing") || !strings.Contains(diagnostic, "inspect") {
			t.Fatalf("%s unknown workflow: %d %q", sub, code, diagnostic)
		}
	}
	code, out, _ := invoke(t, "", "pipeline", "--help")
	if code != 0 || !strings.Contains(out, "--workflow") || !strings.Contains(out, "Broodfile") {
		t.Fatalf("help: %d %q", code, out)
	}
}

func TestPipelineValidatePlanRender(t *testing.T) {
	isolateState(t)
	path := writePipeline(t)
	// validate: только INFO без ERROR, код 0.
	code, out, diagnostic := invoke(t, "", "pipeline", "validate", path)
	if code != 0 || strings.Contains(out, "ERROR") || diagnostic != "" {
		t.Fatalf("validate: %d %q %q", code, out, diagnostic)
	}
	if !strings.Contains(out, "WARNING") && !strings.Contains(out, "INFO") {
		t.Fatalf("validate must print diagnostics: %q", out)
	}
	// plan: последовательность узлов до exit.
	code, out, diagnostic = invoke(t, "", "pipeline", "plan", path)
	if code != 0 || diagnostic != "" {
		t.Fatalf("plan: %d %q %q", code, out, diagnostic)
	}
	if !strings.HasPrefix(out, "prepare -> review\n") || !strings.Contains(out, "exit at done") {
		t.Fatalf("plan output: %q", out)
	}
	// render --dot: валидный DOT.
	code, out, diagnostic = invoke(t, "", "pipeline", "render", path, "--dot")
	if code != 0 || diagnostic != "" || !strings.HasPrefix(out, "digraph pipeline {") {
		t.Fatalf("render: %d %q %q", code, out, diagnostic)
	}
	if !strings.Contains(out, `prepare -> review;`) || !strings.Contains(out, `land -> done;`) {
		t.Fatalf("render must list edges: %q", out)
	}
}

func TestPipelineValidateReportsErrors(t *testing.T) {
	isolateState(t)
	// Ровно одна ERROR-строка и ненулевой код: exit без входящих рёбер.
	text := strings.Replace(validPipelineText(t),
		"  - {from: land, to: done}\n", "", 1)
	path := writePipelineText(t, text)
	code, out, diagnostic := invoke(t, "", "pipeline", "validate", path)
	if code == 0 || !strings.Contains(out, "ERROR") {
		t.Fatalf("validate must fail with errors: %d %q %q", code, out, diagnostic)
	}
	// plan того же файла отказывает с причиной.
	code, _, diagnostic = invoke(t, "", "pipeline", "plan", path)
	if code == 0 || diagnostic == "" {
		t.Fatalf("plan must reject a broken graph: %d %q", code, diagnostic)
	}
}

func TestPipelineRejectsBadArguments(t *testing.T) {
	isolateState(t)
	path := writePipeline(t)
	for _, args := range [][]string{
		{"pipeline"},
		{"pipeline", "unknown", path},
		{"pipeline", "validate"},
		{"pipeline", "validate", path, path},
		{"pipeline", "validate", path, "--dot"},
		{"pipeline", "validate", "--workflow"},
		{"pipeline", "plan", "--workflow", ""},
		{"pipeline", "validate", "--workflow", "a", "--workflow", "b", path},
		{"pipeline", "plan", path, "--dot"},
		{"pipeline", "render", path},
		{"pipeline", "render", path, "--dot", "--extra"},
		{"pipeline", "render", path, "--svg", "--dot"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, diagnostic := invoke(t, "", args...)
			if code == 0 || diagnostic == "" {
				t.Fatalf("code=%d out=%q err=%q", code, out, diagnostic)
			}
		})
	}
}

func TestPipelinePlanDetectsCycle(t *testing.T) {
	isolateState(t)
	text := strings.Replace(validPipelineText(t),
		"  - {from: check, to: land, condition: \"context.report=ok\"}\n",
		"  - {from: check, to: land, condition: \"context.report=ok\"}\n  - {from: build, to: build, weight: 10}\n", 1)
	path := writePipelineText(t, text)
	code, out, diagnostic := invoke(t, "", "pipeline", "plan", path)
	if code == 0 || !strings.Contains(diagnostic, "cycle") {
		t.Fatalf("plan must report the cycle: %d %q %q", code, out, diagnostic)
	}
}

// FEATURE: Render pipeline graphs as SVG.
// SPEC: Standalone documents and Broodfile workflows render with --svg.
// IMPLEMENTATION: The CLI uses the shared document graph renderer.
// RESULT: IMPLEMENTED — both document forms support SVG.
// STATUS: DONE
func TestPipelineRenderSVG(t *testing.T) {
	isolateState(t)
	for _, args := range [][]string{{"pipeline", "render", writePipeline(t), "--svg"}, {"pipeline", "render", "--workflow", "inspect", filepath.Join("..", "..", "..", "examples", "Broodfile"), "--svg"}} {
		code, out, diagnostic := invoke(t, "", args...)
		if code != 0 || !strings.Contains(out, "<svg ") || !strings.Contains(out, "node-start") {
			t.Fatalf("%v: %d %q %q", args, code, out, diagnostic)
		}
	}
}
