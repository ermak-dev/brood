package pipeline

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// BUG: anchors on mapping keys bypass structural validation.
// HYPOTHESIS: mapping keys are checked directly without inspecting their anchors.
// SOLUTION: reject anchored keys after the existing key and duplicate checks.
// RESULT: FIXED — anchored keys are rejected without changing earlier diagnostics.
// STATUS: FIXED
func TestMappingKeyAnchors(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"root", "apiVersion: brood.dev/v1alpha1\n&foo name: x\n", "YAML anchors and aliases are forbidden"},
		{"nested", "apiVersion: brood.dev/v1alpha1\nnodes: {&foo x: {type: actor}}\n", "YAML anchors and aliases are forbidden"},
		{"ordinary", "apiVersion: brood.dev/v1alpha1\nname: x\n", ""},
		{"alias key", "apiVersion: brood.dev/v1alpha1\nname: &foo x\n*foo: y\n", "YAML anchors and aliases are forbidden"},
		{"merge key", "apiVersion: brood.dev/v1alpha1\n'<<': x\n", "YAML merge keys are forbidden"},
		{"anchored merge key", "apiVersion: brood.dev/v1alpha1\n&foo '<<': x\n", "YAML merge keys are forbidden"},
		{"duplicate before anchor", "apiVersion: brood.dev/v1alpha1\nname: x\n&foo name: y\n", "duplicate mapping key \"name\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeDocument(tc.text, "test.yaml", true)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			want := "test.yaml: document (line 1): " + tc.want
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v; want %s", err, want)
			}
		})
	}
}

// BUG: strict decoding compares every pair of mapping keys after validation.
// HYPOTHESIS: yaml.v3 repeats its quadratic duplicate-key check during decoding.
func TestDecodeStrictCompatibility(t *testing.T) {
	cases := []string{
		"apiVersion: brood.dev/v1alpha1\nname: example\nnodes: {a: {type: actor}}\n",
		"apiVersion: brood.dev/v1alpha1\nname: 12\nstart: true\nexit: 1.5\n",
		"apiVersion: brood.dev/v1alpha1\nname: null\nretry: null\nedges: null\n",
		"apiVersion: brood.dev/v1alpha1\nretry: {maxRetries: 0x10}\nedges: [{weight: 1.5}]\n",
		"apiVersion: brood.dev/v1alpha1\nretry: {maxRetries: '12'}\n",
		"apiVersion: brood.dev/v1alpha1\nunknown: 1\nretry: {bad: true, maxRetries: nope}\n",
		"apiVersion: brood.dev/v1alpha1\nnodes: {one: {options: [{label: [x]}, {oops: 1}]}, two: {type: {x: y}}}\n",
		"apiVersion: brood.dev/v1alpha1\ngoalGates: [one, 2, null, true]\nstyles: [{selectors: [x, 3]}]\n",
		"apiVersion: brood.dev/v1alpha1\nnodes: [a, b]\nedges: {from: x}\n",
		"apiVersion: brood.dev/v1alpha1\nname: 'null'\nretry: {maxRetries: -2}\n",
		"apiVersion: brood.dev/v1alpha1\nretry: {maxRetries: 0o17}\nedges: [{weight: 0x10}]\n",
		"apiVersion: brood.dev/v1alpha1\nretry: {maxRetries: .inf}\nname: false\n",
		"apiVersion: brood.dev/v1alpha1\nretry: {maxRetries: null, missing: 1}\nunknown: true\n",
		"apiVersion: brood.dev/v1alpha1\nnodes: {x: {options: [{bad: 1, outcome: [x]}]}, y: null}\n",
		"apiVersion: brood.dev/v1alpha1\nstyles: [{selectors: [null, yes, 0x10, 1.25]}]\n",
	}
	for _, scalar := range []string{"0", "-1", "012", "0o17", "0x10", "1.5", "1e3", ".inf", ".nan", "null", "~", "true", "false", "yes", "'12'", "\"null\"", "!!str 12", "!!int nope", "[]", "{}"} {
		for _, field := range []string{"name: %s", "retry: {maxRetries: %s}", "edges: [{weight: %s}]", "nodes: {x: {options: [{label: %s}]}}", "goalGates: [%s]"} {
			cases = append(cases, "apiVersion: brood.dev/v1alpha1\n"+fmt.Sprintf(field, scalar)+"\n")
		}
	}
	for i, text := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			decoder := yaml.NewDecoder(strings.NewReader(text))
			decoder.KnownFields(true)
			var want Document
			wantErr := decoder.Decode(&want)
			got, gotErr := decodeDocument(text, "test.yaml", true)
			if wantErr != nil {
				wantMessage := fmt.Sprintf("test.yaml: document (line %d): invalid pipeline document: %s", yamlErrorLine(wantErr), wantErr)
				if gotErr == nil || gotErr.Error() != wantMessage {
					t.Fatalf("error = %v; want %s", gotErr, wantMessage)
				}
				return
			}
			if gotErr != nil || !reflect.DeepEqual(got, &want) {
				t.Fatalf("document = %#v, error = %v; want %#v", got, gotErr, want)
			}
		})
	}
}

// BUG: a second stream decoder repeats yaml.v3's quadratic duplicate checks.
// HYPOTHESIS: decoding the already checked node avoids that second scan.
// SOLUTION: walk checked collections and delegate scalar conversions to yaml.v3.
// RESULT: FIXED — the strict path does not parse the stream twice.
// STATUS: FIXED
func TestDecodeUsesSingleYAMLStream(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "decode.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewDecoder" {
			return true
		}
		if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == "yaml" {
			count++
		}
		return true
	})
	if count != 1 {
		t.Fatalf("got %d YAML stream decoder constructions, want one", count)
	}
}

func FuzzCheckedYAMLCompatibility(f *testing.F) {
	for _, seed := range []string{
		"name: .inf\nretry: {maxRetries: 0x10}\n", "nodes: {x: {options: [{bad: 1, label: []}]}}\n",
		"goalGates: [null, true, 012, 'false']\n", "retry: !!null {bad: x}\n", "nodes: !!str {x: y}\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 8192 {
			return
		}
		var root yaml.Node
		if yaml.Unmarshal([]byte(text), &root) != nil || len(root.Content) == 0 || checkYAMLNodes(&root, 0) != nil {
			return
		}
		var want, got Document
		decoder := yaml.NewDecoder(strings.NewReader(text))
		decoder.KnownFields(true)
		wantErr := decoder.Decode(&want)
		gotErr := decodeCheckedYAML(root.Content[0], &got)
		if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || (wantErr == nil && !reflect.DeepEqual(got, want)) {
			t.Fatalf("input %q\ngot %#v error %v\nwant %#v error %v", text, got, gotErr, want, wantErr)
		}
	})
}

func BenchmarkDecodeWideMapping(b *testing.B) {
	for _, count := range []int{1000, 2000, 4000, 8000, 16000, 32000, 60000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var input strings.Builder
			input.WriteString("apiVersion: brood.dev/v1alpha1\n")
			for i := 0; i < count; i++ {
				fmt.Fprintf(&input, "unknown%d: 1\n", i)
			}
			text := input.String()
			b.SetBytes(int64(len(text)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = decodeDocument(text, "benchmark.yaml", true)
			}
		})
	}
}
