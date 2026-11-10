package pipeline

// FEATURE: Узел называет роль, а не способ создания актора (#208).
// SPEC: Node.Role обязателен у actor-узлов (DNS label) и запрещён у остальных
// типов; провижининг покинул граф: template, managedPool и styles удалены.
// IMPLEMENTATION: Валидатор проверяет роль в checkSchema и nodeGraphErrors,
// разбор отвергает замещённые поля с именем замены.
// RESULT: IMPLEMENTED — роль обязательна и однозначна, старые поля названы.
// STATUS: DONE

import (
	"strings"
	"testing"
)

// roleGraph — минимальный граф с ролями у actor-узлов.
const roleGraph = `apiVersion: brood.dev/v1alpha1
name: roles
start: work
exit: done
nodes:
  work: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: work, to: done}
goalGates: [done]
`

// TestRoleRequiredOnActor — actor-узел без роли отвергается разбором.
func TestRoleRequiredOnActor(t *testing.T) {
	text := strings.Replace(roleGraph, "role: build, ", "", 1)
	if _, err := Parse(text, "roles.yaml"); err == nil {
		t.Fatal("actor node without a role was accepted")
	}
}

// TestRoleForbiddenOnNonActor — роль у human- и exit-узла отвергается.
func TestRoleForbiddenOnNonActor(t *testing.T) {
	for _, tc := range []struct{ name, replace, with string }{
		{"human", "done: {type: exit}", "ask: {type: human, role: build, question: q?, options: [{label: y, outcome: success}]}\n  done: {type: exit}"},
		{"exit", "done: {type: exit}", "done: {type: exit, role: build}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Replace(roleGraph, tc.replace, tc.with, 1)
			if _, err := Parse(text, "roles.yaml"); err == nil {
				t.Fatalf("%s node with a role was accepted", tc.name)
			}
		})
	}
}

// TestRoleMustBeDNSLabel — некорректная роль отвергается.
func TestRoleMustBeDNSLabel(t *testing.T) {
	for _, role := range []string{"Bad_Role", "-lead", "lead-", "", "two words"} {
		text := strings.Replace(roleGraph, "role: build", "role: "+role, 1)
		if _, err := Parse(text, "roles.yaml"); err == nil {
			t.Fatalf("role %q was accepted", role)
		}
	}
}

// TestProgrammaticRoleValidation — роль проверяется и без разбора YAML.
func TestProgrammaticRoleValidation(t *testing.T) {
	d, err := Parse(roleGraph, "roles.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, node := range map[string]Node{
		"missing role":      {Type: "actor", Prompt: "work"},
		"malformed role":    {Type: "actor", Role: "Bad_Role", Prompt: "work"},
		"role on exit node": {Type: "exit", Role: "build"},
	} {
		d.Nodes["work"] = node
		if node.Type == "exit" {
			d.Nodes["work"] = Node{Type: "actor", Role: "build", Prompt: "work"}
			d.Nodes["done"] = node
		}
		if err := d.Validate(); err == nil {
			t.Fatalf("%s accepted by Validate", name)
		}
		d.Nodes["done"] = Node{Type: "exit"}
	}
}

// TestRolesOfDocument — набор ролей документа — производная величина.
func TestRolesOfDocument(t *testing.T) {
	d, err := Parse(roleGraph, "roles.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := d.Roles()
	if len(got) != 1 || got["build"] != true {
		t.Fatalf("roles: %v", got)
	}
}

// TestLegacyProvisioningFieldsRejected — template и managedPool отвергаются
// отказом, который называет само поле и заменяющее его role; styles
// отвергается общей строгостью разбора как неизвестное поле. Имя файла без
// слова role, чтобы подстрока не совпала с префиксом отказа.
func TestLegacyProvisioningFieldsRejected(t *testing.T) {
	for _, tc := range []struct {
		name, replace, with, want string
	}{
		{"template instead of role", "role: build", "template: coding", `nodes.work.template: removed; name the actor's role with "role"`},
		{"managedPool instead of role", "role: build", "managedPool: pool", `nodes.work.managedPool: removed; name the actor's role with "role"`},
		{"template beside role", "role: build", "role: build, template: coding", `nodes.work.template: removed`},
		{"managedPool beside role", "role: build", "role: build, managedPool: pool", `nodes.work.managedPool: removed`},
		{"styles", "goalGates: [done]", "styles: [{selectors: [\"*\"], template: coding}]\ngoalGates: [done]", `field styles not found in type pipeline.Document`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Replace(roleGraph, tc.replace, tc.with, 1)
			if text == roleGraph {
				t.Fatal("test replacement did not change the document")
			}
			_, err := Parse(text, "graph.yaml")
			if err == nil {
				t.Fatalf("legacy %s accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error must contain %q: %v", tc.want, err)
			}
		})
	}
}

// TestSupportedAcceptsRoles — движок больше не отвергает роль актора.
func TestSupportedAcceptsRoles(t *testing.T) {
	d, err := Parse(roleGraph, "roles.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := Supported(d); err != nil {
		t.Fatalf("role actor rejected: %v", err)
	}
}
