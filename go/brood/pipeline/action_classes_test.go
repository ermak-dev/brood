package pipeline

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func actionGraph(classes string) string {
	return fmt.Sprintf(`apiVersion: brood.dev/v1alpha1
name: policy-test
start: work
exit: done
nodes:
  work:
    type: actor
    role: build
%s  done: {type: exit}
edges:
  - {from: work, to: done}
goalGates: [done]
`, classes)
}

func TestNodeActionClassesAreExplicitAndRoundTrip(t *testing.T) {
	document, err := Parse(actionGraph("    actionClasses: [schema, deploy]\n"), "workflow")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(document.Nodes["work"].ActionClasses, []string{"schema", "deploy"}) {
		t.Fatal("declared actions lost")
	}
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Parse(string(raw), "workflow")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Nodes["work"].ActionClasses, document.Nodes["work"].ActionClasses) {
		t.Fatal("actions not pinned in canonical graph")
	}
	legacy, err := Parse(actionGraph(""), "workflow")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Nodes["work"].ActionClasses != nil {
		t.Fatal("unclassified legacy node became work implicitly")
	}
	for _, source := range []string{actionGraph(""), actionGraph("    actionClasses: null\n")} {
		unknown, err := Parse(source, "workflow")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := yaml.Marshal(unknown)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := Parse(string(raw), "workflow")
		if err != nil || restored.Nodes["work"].ActionClasses != nil {
			t.Fatal("unknown classification changed during serialization")
		}
	}
}

func TestNodeActionClassesRejectAmbiguity(t *testing.T) {
	for _, classes := range []string{"[]", "[unknown]", "[landing, landing]", "[work, landing]", "[schema, deploy, landing, review, destructive_lifecycle, work, schema]", "schema"} {
		if _, err := Parse(actionGraph("    actionClasses: "+classes+"\n"), "workflow"); err == nil {
			t.Fatalf("invalid classification accepted: %s", classes)
		}
	}
	bad := strings.Replace(actionGraph(""), "done: {type: exit}", "done: {type: exit, actionClasses: [landing]}", 1)
	if _, err := Parse(bad, "workflow"); err == nil {
		t.Fatal("non-actor classification accepted")
	}
}
