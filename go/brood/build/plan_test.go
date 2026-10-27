// Проверки чистого разбора Broodfile и разрешения плана сборки.
// Портированы из brood/dev/build/tests/test_plan.py и test_resources.py.
package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// minimal читает фикстуру и возвращает независимый разбор.
func minimal(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := ParseDocument(string(data), "minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// planned строит план фикстуры с контекстом по умолчанию.
func planned(t *testing.T, document map[string]any, opts planOptions) map[string]any {
	t.Helper()
	if document == nil {
		document = minimal(t)
	}
	if opts.registry == "" {
		opts.registry = "localhost:5000/team"
	}
	if opts.atespace == "" {
		opts.atespace = "development"
	}
	plan, err := MakePlan(document, opts.pins, opts.target, opts.registry, opts.atespace, opts.filename)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

type planOptions struct {
	pins     map[string]string
	target   string
	registry string
	atespace string
	filename string
}

// wantError ждёт отказа с частью сообщения.

func marshal(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func replace(document map[string]any, path []any, value any) {
	current := any(document)
	for _, part := range path[:len(path)-1] {
		switch node := current.(type) {
		case map[string]any:
			current = node[part.(string)]
		case []any:
			current = node[part.(int)]
		}
	}
	last := path[len(path)-1]
	switch node := current.(type) {
	case map[string]any:
		node[last.(string)] = value
	case []any:
		node[last.(int)] = value
	}
}

func parseCases(t *testing.T, texts []string) {
	t.Helper()
	for _, text := range texts {
		_, err := ParseDocument(text, "custom.yaml")
		if err == nil {
			t.Errorf("ParseDocument must reject %q", text)
			continue
		}
		if !strings.Contains(err.Error(), "custom.yaml") {
			t.Errorf("error %q must name custom.yaml", err.Error())
		}
		if !strings.Contains(err.Error(), "line") {
			t.Errorf("error %q must contain a line", err.Error())
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("error %q must not disclose secret-value", err.Error())
		}
	}
}

func TestMinimalIndependentPlainJSONDocument(t *testing.T) {
	first, second := minimal(t), minimal(t)
	first["sources"].(map[string]any)["project"].(map[string]any)["destination"] = "changed"
	if got := second["sources"].(map[string]any)["project"].(map[string]any)["destination"]; got != "project" {
		t.Fatalf("second parse must stay independent, got %v", got)
	}
	remarshaled := map[string]any{}
	if err := json.Unmarshal([]byte(marshal(t, second)), &remarshaled); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remarshaled, second) {
		t.Fatalf("document must round-trip JSON: %s", marshal(t, second))
	}
}

func TestYAMLFeaturesRejectedWithoutDisclosingValues(t *testing.T) {
	parseCases(t, []string{
		"name: secret-value\nname: duplicate\n",
		"name: &secret-value foo\n",
		"name: *secret-value\n",
		"name: {<<: {secret-value: test}}\n",
		"name: !!python/object:secret-value {}\n",
		"name: !secret-value foo\n",
		"1: secret-value\n",
		"? [secret-value]\n: test\n",
		"name: [secret-value\n",
		"---\nname: secret-value\n---\nname: second\n",
	})
}

func TestSchemaErrorsAreSanitizedAndLocated(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, invalid := range []string{
		strings.Replace(text, "name: example", "name: secret-value!", 1),
		strings.Replace(text, "platform: linux/amd64", "platform: secret-value", 1),
		text + "secret-value: hidden\n",
		strings.Replace(text, "command: [python3]", "command: [python3]\n          image: secret-value", 1),
		strings.Replace(text, "type: project", "type: project\n    repository: secret-value", 1),
	} {
		_, err := ParseDocument(invalid, "test.yaml")
		if err == nil {
			t.Errorf("ParseDocument must reject the mutated document")
			continue
		}
		if !strings.Contains(err.Error(), "test.yaml") || !strings.Contains(err.Error(), "line") {
			t.Errorf("error %q must be located", err.Error())
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("error %q must not disclose secret-value", err.Error())
		}
	}
}

func TestEmptyWrongTypesSizeAndNestingAreErrors(t *testing.T) {
	for _, text := range []string{"", "null", "[]", "name: true",
		"x" + strings.Repeat("x", 1024*1024),
		"#" + strings.Repeat("é", 512*1024),
		strings.Repeat("[", 1500) + strings.Repeat("]", 1500)} {
		if _, err := ParseDocument(text, "Broodfile"); err == nil {
			t.Errorf("ParseDocument must reject %d-byte input", len(text))
		}
	}
}

func TestDuplicateAndNonStringNestedKeys(t *testing.T) {
	for _, text := range []string{
		"name: {a: 1, a: 2}",
		"name: {true: value}",
		"name: {null: value}",
	} {
		if _, err := ParseDocument(text, "Broodfile"); err == nil {
			t.Errorf("ParseDocument must reject %q", text)
		}
	}
}

func TestSafePath(t *testing.T) {
	got, err := SafePath("project//./Dockerfile", "path")
	if err != nil || got != "project/Dockerfile" {
		t.Fatalf("SafePath normalization: %q, %v", got, err)
	}
	got, err = SafePath("a b/file", "path")
	if err != nil || got != "a b/file" {
		t.Fatalf("SafePath spaces: %q, %v", got, err)
	}
	for _, value := range []string{"", ".", "./", "/absolute", "../x", "a/../x", `a\b`, "a\nb",
		"a\x00b", "a\x7fb", "C:/file"} {
		if _, err := SafePath(value, "path"); err == nil {
			t.Errorf("SafePath must reject %q", value)
		}
	}
}

func TestSafeRepositories(t *testing.T) {
	for _, value := range []string{
		"https://github.com/team/repo.git",
		"ssh://git@github.com:2222/team/repo",
		"git@github.com:team/repo.git",
	} {
		got, err := Repository(value)
		if err != nil || got != value {
			t.Errorf("Repository(%q) = %v, %v", value, got, err)
		}
	}
	for _, value := range []string{
		"https://user:secret-value@github.com/repo",
		"https://secret-value@host/repo",
		"ssh://git:secret-value@host/repo",
		"https://host/repo?secret-value",
		"https://host/repo#secret-value",
		"file:///secret-value",
		"ext::secret-value",
		"../secret-value",
		"http://host/repo",
		"git://host/repo",
		"https://host",
		"https://host/repo\nsecret-value",
		"git@host:../secret-value",
	} {
		_, err := Repository(value)
		if err == nil {
			t.Errorf("Repository must reject %q", value)
			continue
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("error %q must not disclose secret-value", err.Error())
		}
	}
}

func TestPublicHostnames(t *testing.T) {
	got, err := PublicHostname("api.github.com")
	if err != nil || got != "api.github.com" {
		t.Fatalf("PublicHostname: %q, %v", got, err)
	}
	for _, value := range []string{"localhost", "a.localhost", "a.local", "a.internal", "a.lan", "a.home",
		"a.test", "a.invalid", "a.example", "a.localdomain", "a.intranet",
		"127.0.0.1", "[::1]", "*.github.com",
		"https://github.com", "github.com:443", "bad_.com", "-bad.com", "a..com",
		"github.com.", "10.0.0.1", "kubernetes.default.svc.cluster.local"} {
		if _, err := PublicHostname(value); err == nil {
			t.Errorf("PublicHostname must reject %q", value)
		}
	}
}

func TestExactMinimalPlan(t *testing.T) {
	result := planned(t, nil, planOptions{})
	want := map[string]any{
		"apiVersion": "brood.dev/v1alpha1", "project": "example",
		"target": "development", "environment": "python",
		"sources": map[string]any{"project": map[string]any{
			"type": "project", "destination": "project", "gitMetadata": false}},
		"build":    map[string]any{"dockerfile": "project/environment/Dockerfile", "platform": "linux/amd64", "arguments": map[string]any{}},
		"actor":    minimal(t)["templates"].(map[string]any)["python"].(map[string]any)["actor"],
		"network":  map[string]any{},
		"registry": "localhost:5000/team", "atespace": "development", "pins": map[string]any{},
		"pinsFile": nil,
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("plan mismatch:\ngot  %s\nwant %s", marshal(t, result), marshal(t, want))
	}
	remarshaled := map[string]any{}
	if err := json.Unmarshal([]byte(marshal(t, result)), &remarshaled); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remarshaled, result) {
		t.Fatal("plan must round-trip JSON")
	}
}

func TestSharedEnvironmentSeparateRuntimeAndIndependentOutputs(t *testing.T) {
	document := minimal(t)
	coding := deepCopy(t, document["templates"].(map[string]any)["python"]).(map[string]any)
	coding["actor"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"] = []any{"-i"}
	coding["network"] = "internet"
	document["templates"].(map[string]any)["coding"] = coding
	document["networks"].(map[string]any)["internet"] = map[string]any{
		"tls": map[string]any{"hosts": []any{"github.com"}, "ports": []any{443}},
	}
	document["targets"].(map[string]any)["coding"] = map[string]any{"template": "coding"}
	original := deepCopy(t, document)
	first := planned(t, document, planOptions{})
	second := planned(t, document, planOptions{target: "coding"})
	if !reflect.DeepEqual(first["build"], second["build"]) {
		t.Fatal("shared environment must give equal build")
	}
	if !reflect.DeepEqual(first["sources"], second["sources"]) {
		t.Fatal("shared environment must give equal sources")
	}
	if reflect.DeepEqual(first["actor"], second["actor"]) {
		t.Fatal("actor must differ")
	}
	if reflect.DeepEqual(first["network"], second["network"]) {
		t.Fatal("network must differ")
	}
	second["actor"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"] = []any{"changed"}
	second["network"].(map[string]any)["tls"].(map[string]any)["hosts"] = []any{"example.org"}
	second["sources"].(map[string]any)["project"].(map[string]any)["destination"] = "changed"
	if marshal(t, document) != marshal(t, original) {
		t.Fatal("plan must not mutate the document")
	}
	if !reflect.DeepEqual(first, planned(t, document, planOptions{})) {
		t.Fatal("repeated plan must be equal")
	}
}

func TestPinsResolvedAndOnlySelectedPinsReturned(t *testing.T) {
	document := minimal(t)
	document["pinsFile"] = "./pins.json"
	document["sources"].(map[string]any)["upstream"] = map[string]any{
		"type": "git", "destination": "upstream", "repository": "https://github.com/team/repo",
		"revisionPin": "SOURCE_REVISION", "gitMetadata": true,
	}
	document["environments"].(map[string]any)["python"].(map[string]any)["sources"] = []any{"project", "upstream"}
	build := document["environments"].(map[string]any)["python"].(map[string]any)["build"].(map[string]any)
	build["arguments"] = map[string]any{"MODE": "release"}
	build["argsFromPins"] = []any{"VERSION"}
	pins := map[string]string{"SOURCE_REVISION": strings.Repeat("a", 40), "VERSION": "3.12", "UNUSED": "unused"}
	result := planned(t, document, planOptions{pins: pins})
	wantPins := map[string]any{"SOURCE_REVISION": strings.Repeat("a", 40), "VERSION": "3.12"}
	if !reflect.DeepEqual(result["pins"], wantPins) {
		t.Fatalf("pins: %s", marshal(t, result["pins"]))
	}
	if got := result["sources"].(map[string]any)["upstream"].(map[string]any)["revision"]; got != strings.Repeat("a", 40) {
		t.Fatalf("revision: %v", got)
	}
	if _, ok := result["sources"].(map[string]any)["upstream"].(map[string]any)["revisionPin"]; ok {
		t.Fatal("revisionPin must not survive into the plan")
	}
	wantArguments := map[string]any{"MODE": "release", "VERSION": "3.12"}
	if !reflect.DeepEqual(result["build"].(map[string]any)["arguments"], wantArguments) {
		t.Fatalf("arguments: %s", marshal(t, result["build"]))
	}
	if result["pinsFile"] != "pins.json" {
		t.Fatalf("pinsFile: %v", result["pinsFile"])
	}
	if _, ok := document["sources"].(map[string]any)["upstream"].(map[string]any)["revisionPin"]; !ok {
		t.Fatal("document revisionPin must survive")
	}
	result["pins"].(map[string]any)["VERSION"] = "changed"
	if pins["VERSION"] != "3.12" {
		t.Fatal("plan must not share memory with inputs")
	}
}

func TestInvalidFieldsAndReferences(t *testing.T) {
	cases := []struct {
		path  []any
		value any
	}{
		{[]any{"defaultTarget"}, "missing"},
		{[]any{"targets", "development", "template"}, "missing"},
		{[]any{"templates", "python", "environment"}, "missing"},
		{[]any{"templates", "python", "network"}, "missing"},
		{[]any{"environments", "python", "sources"}, []any{"missing"}},
		{[]any{"environments", "python", "sources"}, []any{"project", "project"}},
		{[]any{"sources", "project", "destination"}, "../bad"},
		{[]any{"pinsFile"}, "../pins.json"},
		{[]any{"environments", "python", "build", "dockerfile"}, "other/Dockerfile"},
		{[]any{"environments", "python", "build", "dockerfile"}, "project"},
		{[]any{"environments", "python", "build", "arguments"}, map[string]any{"API_TOKEN": "secret-value"}},
		{[]any{"templates", "python", "actor", "resources", "limits", "memoryBytes"}, "18446744073709551616"},
		{[]any{"templates", "python", "actor", "containers", 0, "env"},
			[]any{map[string]any{"name": "A", "value": "1"}, map[string]any{"name": "A", "value": "2"}}},
		{[]any{"templates", "python", "actor", "volumes"},
			[]any{map[string]any{"name": "data", "durableDir": map[string]any{}}, map[string]any{"name": "data", "durableDir": map[string]any{}}}},
		{[]any{"templates", "python", "actor", "containers", 0, "volumeMounts"},
			[]any{map[string]any{"name": "missing", "mountPath": "/data"}}},
	}
	for _, item := range cases {
		document := minimal(t)
		replace(document, item.path, item.value)
		_, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "custom.yaml")
		if err == nil {
			t.Errorf("MakePlan must reject path %v", item.path)
			continue
		}
		if !strings.Contains(err.Error(), "custom.yaml") {
			t.Errorf("error %q must name custom.yaml", err.Error())
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("error %q must not disclose secret-value", err.Error())
		}
	}
}

func TestAllUnselectedDefinitionsAreValidated(t *testing.T) {
	actor := deepCopy(t, minimal(t)["templates"].(map[string]any)["python"].(map[string]any)["actor"])
	for section, value := range map[string]any{
		"targets":      map[string]any{"template": "missing"},
		"templates":    map[string]any{"environment": "missing", "network": "offline", "actor": actor},
		"environments": map[string]any{"sources": []any{"missing"}, "build": map[string]any{"dockerfile": "project/Dockerfile", "platform": "linux/amd64"}},
		"networks":     map[string]any{"tls": map[string]any{"hosts": []any{"localhost"}, "ports": []any{443}}},
		"sources": map[string]any{"type": "git", "destination": "other", "revisionPin": "UNKNOWN",
			"repository": "https://github.com/team/repo"},
	} {
		document := minimal(t)
		document[section].(map[string]any)["unused"] = value
		if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("MakePlan must validate unselected %s", section)
		}
	}
}

func TestSourceDestinationsDoNotOverlapAndProjectIsUnique(t *testing.T) {
	for _, destination := range []string{"project", "project/sub", "./project//sub", "other"} {
		document := minimal(t)
		document["sources"].(map[string]any)["second"] = map[string]any{"type": "project", "destination": destination}
		if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("overlapping destination %q must be rejected", destination)
		}
	}
	document := minimal(t)
	document["sources"].(map[string]any)["second"] = map[string]any{
		"type": "git", "destination": "project/sub",
		"repository": "git@github.com:team/repo", "revision": strings.Repeat("a", 40)}
	if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
		t.Fatal("nested destination must be rejected")
	}
}

func TestInvalidPins(t *testing.T) {
	for name, pins := range map[string]map[string]string{
		"bad-name": {"bad": "v1"},
		"too-long": {"VERSION": strings.Repeat("x", 16385)},
		"control":  {"VERSION": "secret-value\x00"},
	} {
		if _, err := MakePlan(minimal(t), pins, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("MakePlan must reject pins %s", name)
		}
	}
	document := minimal(t)
	document["pinsFile"] = "pins.json"
	document["environments"].(map[string]any)["python"].(map[string]any)["build"].(map[string]any)["argsFromPins"] = []any{"MISSING"}
	for name, pins := range map[string]map[string]string{"empty": {}, "present": {"VERSION": "v1"}} {
		if _, err := MakePlan(document, pins, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("MakePlan must reject missing pin %s", name)
		}
	}
}

func TestArgumentCollisionAndSecretPinNames(t *testing.T) {
	for _, item := range []struct {
		arguments map[string]any
		fromPins  []any
	}{
		{map[string]any{"VERSION": "a"}, []any{"VERSION"}},
		{map[string]any{}, []any{"AUTH_KEY"}},
	} {
		document := minimal(t)
		document["pinsFile"] = "pins.json"
		build := document["environments"].(map[string]any)["python"].(map[string]any)["build"].(map[string]any)
		build["arguments"] = item.arguments
		build["argsFromPins"] = item.fromPins
		pins := map[string]string{"VERSION": "b", "AUTH_KEY": "secret-value"}
		if _, err := MakePlan(document, pins, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("MakePlan must reject arguments %v pins %v", item.arguments, item.fromPins)
		}
	}
}

func TestRevisionPinsAndPinFileDeclaration(t *testing.T) {
	for _, revision := range []string{"short", strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		document := minimal(t)
		document["pinsFile"] = "pins.json"
		document["sources"].(map[string]any)["upstream"] = map[string]any{
			"type": "git", "destination": "upstream", "revisionPin": "REVISION",
			"repository": "https://github.com/team/repo",
		}
		if _, err := MakePlan(document, map[string]string{"REVISION": revision}, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("revision %q must be rejected", revision)
		}
	}
	document := minimal(t)
	document["environments"].(map[string]any)["python"].(map[string]any)["build"].(map[string]any)["argsFromPins"] = []any{"VERSION"}
	if _, err := MakePlan(document, map[string]string{"VERSION": "1.0"}, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
		t.Fatal("pin without pinsFile must be rejected")
	}
}

func TestRegistryAndAtespaceContext(t *testing.T) {
	if _, err := MakePlan(minimal(t), nil, "", "", "development", "Broodfile"); err == nil || !strings.Contains(err.Error(), "BUILD_REGISTRY") {
		t.Fatal("empty registry must demand BUILD_REGISTRY")
	}
	if _, err := MakePlan(minimal(t), nil, "", "localhost:5000/team", "", "Broodfile"); err == nil || !strings.Contains(err.Error(), "BUILD_ATESPACE") {
		t.Fatal("empty atespace must demand BUILD_ATESPACE")
	}
	for _, value := range []string{"https://host/team", "user:secret-value@host/team", "host/team:tag",
		"host/team?secret-value", "host/../team", "host//team", "host:99999/team"} {
		_, err := MakePlan(minimal(t), nil, "", value, "development", "Broodfile")
		if err == nil {
			t.Errorf("registry %q must be rejected", value)
			continue
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("error %q must not disclose secret-value", err.Error())
		}
	}
	for _, value := range []string{"UPPER", "bad.space", "bad_underscore", strings.Repeat("a", 64)} {
		if _, err := MakePlan(minimal(t), nil, "", "localhost:5000/team", value, "Broodfile"); err == nil {
			t.Errorf("atespace %q must be rejected", value)
		}
	}
	if _, err := MakePlan(minimal(t), nil, "unknown", "localhost:5000/team", "development", "Broodfile"); err == nil {
		t.Fatal("unknown target must be rejected")
	}
}

func TestVolumeMountsValidAndDuplicateMountsRefused(t *testing.T) {
	document := minimal(t)
	actor := document["templates"].(map[string]any)["python"].(map[string]any)["actor"].(map[string]any)
	actor["volumes"] = []any{map[string]any{"name": "data", "durableDir": map[string]any{}}}
	container := actor["containers"].([]any)[0].(map[string]any)
	container["volumeMounts"] = []any{map[string]any{"name": "data", "mountPath": "/data"}}
	result := planned(t, document, planOptions{})
	if !reflect.DeepEqual(result["actor"], actor) {
		t.Fatal("actor with volumes must be planned")
	}
	container["volumeMounts"] = append(container["volumeMounts"].([]any),
		map[string]any{"name": "data", "mountPath": "/other"})
	if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
		t.Fatal("duplicate mounts must be rejected")
	}
}

func TestMaximumUint64IsAccepted(t *testing.T) {
	document := minimal(t)
	actor := firstTemplateActor(t, document)
	actor["resources"].(map[string]any)["limits"].(map[string]any)["memoryBytes"] = "18446744073709551615"
	result := planned(t, document, planOptions{})
	if got := result["actor"].(map[string]any)["resources"].(map[string]any)["limits"].(map[string]any)["memoryBytes"]; got != "18446744073709551615" {
		t.Fatalf("maximum uint64 must be accepted, got %v", got)
	}
}

func TestUnsupportedVolumeMountFieldsAreRefused(t *testing.T) {
	document := minimal(t)
	actor := firstTemplateActor(t, document)
	actor["volumes"] = []any{map[string]any{"name": "data", "durableDir": map[string]any{}}}
	actor["containers"].([]any)[0].(map[string]any)["volumeMounts"] = []any{
		map[string]any{"name": "data", "mountPath": "/data", "readOnly": true}}
	if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
		t.Fatal("readOnly mount field must be rejected")
	}
}

func TestIdentifierAndResourcePatternsRejectFinalNewlines(t *testing.T) {
	for _, kind := range []string{"project", "argument", "resource"} {
		document := minimal(t)
		if kind == "project" {
			document["name"] = document["name"].(string) + "\n"
		} else if kind == "argument" {
			document["environments"].(map[string]any)["python"].(map[string]any)["build"].(map[string]any)["arguments"] = map[string]any{"VERSION\n": "1"}
		} else {
			firstTemplateActor(t, document)["resources"].(map[string]any)["limits"].(map[string]any)["memoryBytes"] = "1024\n"
		}
		if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("trailing newline in %s must be rejected", kind)
		}
	}
}

func TestUint64OverflowIsRefused(t *testing.T) {
	document := minimal(t)
	firstTemplateActor(t, document)["resources"].(map[string]any)["limits"].(map[string]any)["memoryBytes"] = "18446744073709551616"
	_, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile")
	if err == nil || !strings.Contains(err.Error(), "uint64") {
		t.Fatal("uint64 overflow must be named")
	}
}

func firstTemplateActor(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	for _, template := range document["templates"].(map[string]any) {
		return template.(map[string]any)["actor"].(map[string]any)
	}
	t.Fatal("no templates")
	return nil
}

func deepCopy(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	out := any(nil)
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Граничные значения uint64 protojson: максимум принят, переполнение отвергнуто.
func TestUint64Boundaries(t *testing.T) {
	for _, value := range []string{"18446744073709551615", "9223372036854775808", "1"} {
		document := minimal(t)
		firstTemplateActor(t, document)["resources"].(map[string]any)["limits"].(map[string]any)["memoryBytes"] = value
		if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err != nil {
			t.Errorf("uint64 value %q must be accepted: %v", value, err)
		}
	}
	for _, value := range []string{"18446744073709551616", "0", "00", "+1024", "-1", "1.5", "0x1000", "1_000", "1e3", "0777"} {
		document := minimal(t)
		firstTemplateActor(t, document)["resources"].(map[string]any)["limits"].(map[string]any)["memoryBytes"] = value
		if _, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
			t.Errorf("resource value %q must be rejected", value)
		}
	}
}

// Golden: разбор и планы побайтово совпадают с эталонной Python-реализацией.
func TestPythonGoldenParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	document, err := ParseDocument(text, "minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if marshal(t, document) != goldenDocument {
		t.Fatalf("document mismatch:\ngot  %s\nwant %s", marshal(t, document), goldenDocument)
	}
	plan, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	if marshal(t, plan) != goldenPlan {
		t.Fatalf("plan mismatch:\ngot  %s\nwant %s", marshal(t, plan), goldenPlan)
	}
	withPins := minimal(t)
	withPins["pinsFile"] = "./pins.json"
	withPins["sources"].(map[string]any)["upstream"] = map[string]any{
		"type": "git", "destination": "upstream", "repository": "https://github.com/team/repo",
		"revisionPin": "SOURCE_REVISION", "gitMetadata": true,
	}
	withPins["environments"].(map[string]any)["python"].(map[string]any)["sources"] = []any{"project", "upstream"}
	build := withPins["environments"].(map[string]any)["python"].(map[string]any)["build"].(map[string]any)
	build["arguments"] = map[string]any{"MODE": "release"}
	build["argsFromPins"] = []any{"VERSION"}
	pins := map[string]string{"SOURCE_REVISION": strings.Repeat("a", 40), "VERSION": "3.12", "UNUSED": "unused"}
	pinsPlan, err := MakePlan(withPins, pins, "", "localhost:5000/team", "development", "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	if marshal(t, pinsPlan) != goldenPinsPlan {
		t.Fatalf("pins plan mismatch:\ngot  %s\nwant %s", marshal(t, pinsPlan), goldenPinsPlan)
	}
	remotePlan, err := MakePlan(withPins, pins, "development", "reg.example.com:8443/team/app", "dev-1", "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	if marshal(t, remotePlan) != goldenRemotePlan {
		t.Fatalf("remote plan mismatch:\ngot  %s\nwant %s", marshal(t, remotePlan), goldenRemotePlan)
	}
}

const goldenDocument = `{"apiVersion":"brood.dev/v1alpha1","defaultTarget":"development","environments":{"python":{"build":{"dockerfile":"project/environment/Dockerfile","platform":"linux/amd64"},"sources":["project"]}},"name":"example","networks":{"offline":{}},"sources":{"project":{"destination":"project","type":"project"}},"targets":{"development":{"template":"python"}},"templates":{"python":{"actor":{"containers":[{"args":["--version"],"command":["python3"],"name":"main"}],"resources":{"limits":{"cpuMillicores":"1000","memoryBytes":"1073741824"}}},"environment":"python","network":"offline"}}}`

const goldenPlan = `{"actor":{"containers":[{"args":["--version"],"command":["python3"],"name":"main"}],"resources":{"limits":{"cpuMillicores":"1000","memoryBytes":"1073741824"}}},"apiVersion":"brood.dev/v1alpha1","atespace":"development","build":{"arguments":{},"dockerfile":"project/environment/Dockerfile","platform":"linux/amd64"},"environment":"python","network":{},"pins":{},"pinsFile":null,"project":"example","registry":"localhost:5000/team","sources":{"project":{"destination":"project","gitMetadata":false,"type":"project"}},"target":"development"}`

const goldenPinsPlan = `{"actor":{"containers":[{"args":["--version"],"command":["python3"],"name":"main"}],"resources":{"limits":{"cpuMillicores":"1000","memoryBytes":"1073741824"}}},"apiVersion":"brood.dev/v1alpha1","atespace":"development","build":{"arguments":{"MODE":"release","VERSION":"3.12"},"dockerfile":"project/environment/Dockerfile","platform":"linux/amd64"},"environment":"python","network":{},"pins":{"SOURCE_REVISION":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","VERSION":"3.12"},"pinsFile":"pins.json","project":"example","registry":"localhost:5000/team","sources":{"project":{"destination":"project","gitMetadata":false,"type":"project"},"upstream":{"destination":"upstream","gitMetadata":true,"repository":"https://github.com/team/repo","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"git"}},"target":"development"}`

const goldenRemotePlan = `{"actor":{"containers":[{"args":["--version"],"command":["python3"],"name":"main"}],"resources":{"limits":{"cpuMillicores":"1000","memoryBytes":"1073741824"}}},"apiVersion":"brood.dev/v1alpha1","atespace":"dev-1","build":{"arguments":{"MODE":"release","VERSION":"3.12"},"dockerfile":"project/environment/Dockerfile","platform":"linux/amd64"},"environment":"python","network":{},"pins":{"SOURCE_REVISION":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","VERSION":"3.12"},"pinsFile":"pins.json","project":"example","registry":"reg.example.com:8443/team/app","sources":{"project":{"destination":"project","gitMetadata":false,"type":"project"},"upstream":{"destination":"upstream","gitMetadata":true,"repository":"https://github.com/team/repo","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"git"}},"target":"development"}`
