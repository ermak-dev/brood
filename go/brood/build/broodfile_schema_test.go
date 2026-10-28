package build

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// FEATURE: A published JSON Schema stays in sync with the structural validator (#21).
// SPEC: The artifact is generated from the actual schema nodes, including zero
// bounds and referenced constraints; semantic and YAML checks remain in the compiler.
// RESULT: IMPLEMENTED — the root/minimal recipes pass an independent JSON Schema probe;
// unknown fields/version and nonempty durableDir are rejected by that same artifact.
func TestBroodfileSchemaArtifact(t *testing.T) {
	schema := schemaArtifactNode(schemaRoot)
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["title"] = "Broodfile"
	schema["description"] = "Broodfile structural schema. Reference resolution, safe paths, Git URLs, uint64 resources and YAML syntax restrictions are additionally checked by bro build."
	expected, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const filename = "broodfile.schema.json"
	if os.Getenv("BROOD_UPDATE_SCHEMA") == "1" {
		if err := os.WriteFile(filename, append(expected, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("published schema artifact: %v", err)
	}
	var published map[string]any
	if err := json.Unmarshal(data, &published); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("published schema differs from the compiler; regenerate with BROOD_UPDATE_SCHEMA=1")
	}
}

// schemaArtifactNode is a test-only exporter; it never changes compiler behavior.
func schemaArtifactNode(node *schemaNode) map[string]any {
	out := map[string]any{}
	if node.typ != "" {
		out["type"] = node.typ
	}
	if node.enum != nil {
		out["enum"] = node.enum
	}
	if node.constSet {
		out["const"] = node.constV
	}
	for name, value := range map[string]int{
		"minLength": node.minLen, "maxLength": node.maxLen,
		"minItems": node.minItems, "maxItems": node.maxItems,
		"minProperties": node.minProps,
	} {
		if value != 0 {
			out[name] = value
		}
	}
	for name, value := range map[string]*int64{
		"minimum": node.minP, "maximum": node.maxP, "maxProperties": node.maxProps,
	} {
		if value != nil {
			out[name] = *value
		}
	}
	if node.pattern != nil {
		out["pattern"] = node.pattern.String()
	}
	if node.uniqueItems {
		out["uniqueItems"] = true
	}
	if node.items != nil {
		out["items"] = schemaArtifactNode(node.items)
	}
	if node.propNames != nil {
		out["propertyNames"] = schemaArtifactNode(node.propNames)
	}
	if len(node.required) != 0 {
		out["required"] = node.required
	}
	if node.addlFalse {
		out["additionalProperties"] = false
	} else if node.addlProps != nil {
		out["additionalProperties"] = schemaArtifactNode(node.addlProps)
	}
	if len(node.props) != 0 {
		properties := map[string]any{}
		for _, property := range node.props {
			properties[property.name] = schemaArtifactNode(property.schema)
		}
		out["properties"] = properties
	}
	for name, nodes := range map[string][]*schemaNode{
		"allOf": node.allOf, "anyOf": node.anyOf, "oneOf": node.oneOf,
	} {
		if len(nodes) == 0 && (name != "allOf" || node.ref == nil) {
			continue
		}
		parts := []any{}
		if name == "allOf" && node.ref != nil {
			parts = append(parts, schemaArtifactNode(node.ref))
		}
		for _, child := range nodes {
			parts = append(parts, schemaArtifactNode(child))
		}
		out[name] = parts
	}
	for name, child := range map[string]*schemaNode{"not": node.notS, "if": node.ifS, "then": node.thenS} {
		if child != nil {
			out[name] = schemaArtifactNode(child)
		}
	}
	return out
}
