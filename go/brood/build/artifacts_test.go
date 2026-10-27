// Проверки независимых манифестов и атомарной публикации комплекта.
// Порт brood/dev/build/tests/test_artifacts.py.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// artifactPlan строит план фикстуры, как planned() в Python.
func artifactPlan(t *testing.T) map[string]any {
	t.Helper()
	plan, err := MakePlan(minimal(t), nil, "", "localhost:5000/team", "development", "minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func artifactImage(plan map[string]any, digit string) string {
	return fmt.Sprintf("%s/%s/%s@sha256:%s", plan["registry"], plan["project"],
		plan["environment"], strings.Repeat(digit, 64))
}

func TestRenderIsIndependentAndSetsProtojsonDefaults(t *testing.T) {
	plan := artifactPlan(t)
	before := deepCopy(t, plan).(map[string]any)
	first, err := Render(plan, artifactImage(plan, "a"), nil)
	wantNoError(t, err)
	second, err := Render(plan, artifactImage(plan, "a"), nil)
	wantNoError(t, err)
	if len(first) != 3 || !artifactFiles["actor-template.json"] {
		t.Fatal("bundle must hold exactly three files")
	}
	if marshal(t, first) != marshal(t, second) {
		t.Fatal("renders must be equal")
	}
	actor := first["actor-template.json"].(map[string]any)
	containers := actor["containers"].([]any)
	if containers[0].(map[string]any)["image"] != artifactImage(plan, "a") {
		t.Fatal("image must be set on the container")
	}
	metadata := actor["metadata"].(map[string]any)
	if metadata["atespace"] != plan["atespace"] {
		t.Fatal("metadata atespace")
	}
	name := metadata["name"].(string)
	if !patternDNSLabel63.MatchString(name) {
		t.Fatalf("name %q must be a DNS label", name)
	}
	golden := actor["snapshotConfig"].(map[string]any)["goldenSnapshotConfig"].(map[string]any)
	if golden["mode"] != "GOLDEN_SNAPSHOT_MODE_DISABLED" {
		t.Fatal("golden snapshot must default to disabled")
	}
	egress := first["egress-policy.json"].(map[string]any)
	if !reflectDeepEqual(egress, map[string]any{
		"metadata": map[string]any{"atespace": plan["atespace"], "name": "default"},
		"rules":    []any{},
	}) {
		t.Fatalf("egress: %s", marshal(t, egress))
	}
	// Мутация результата не меняет план; повтор рендера тот же.
	containers[0].(map[string]any)["command"].([]any)[0] = "changed"
	first["build.json"].(map[string]any)["sources"].(map[string]any)["project"].(map[string]any)["destination"] = "changed"
	if marshal(t, plan) != marshal(t, before) {
		t.Fatal("render must not mutate the plan")
	}
	if marshal(t, second) != marshal(t, mustRender(t, plan, artifactImage(plan, "a"))) {
		t.Fatal("second render must be unaffected")
	}
}

func mustRender(t *testing.T, plan map[string]any, image string) map[string]any {
	t.Helper()
	bundle, err := Render(plan, image, nil)
	wantNoError(t, err)
	return bundle
}

func reflectDeepEqual(left, right any) bool {
	return marshalFor(left) == marshalFor(right)
}

func marshalFor(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestExactEgressRulesAndSnapshotSettingsPreserved(t *testing.T) {
	plan := artifactPlan(t)
	plan["network"] = map[string]any{
		"tls":  map[string]any{"hosts": []any{"github.com"}, "ports": []any{443, 8443}},
		"http": map[string]any{"hosts": []any{"deb.debian.org"}, "ports": []any{80}},
	}
	plan["actor"].(map[string]any)["snapshotConfig"] = map[string]any{
		"preferredFidelity": "SNAPSHOT_FIDELITY_MEMORY"}
	result := mustRender(t, plan, artifactImage(plan, "a"))
	rules := result["egress-policy.json"].(map[string]any)["rules"].([]any)
	want := `[{"tlsPassthrough":{"hostnames":["github.com"],"ports":{"numbers":[443,8443]}}},{"http":{"hostnames":["deb.debian.org"],"ports":{"numbers":[80]}}}]`
	if marshalFor(rules) != want {
		t.Fatalf("rules: %s", marshalFor(rules))
	}
	fidelity := result["actor-template.json"].(map[string]any)["snapshotConfig"].(map[string]any)["preferredFidelity"]
	if fidelity != "SNAPSHOT_FIDELITY_MEMORY" {
		t.Fatalf("preferredFidelity: %v", fidelity)
	}
}

func TestTemplateHashChangesForRuntimeNetworkImageAndIdentity(t *testing.T) {
	plan := artifactPlan(t)
	original := mustRender(t, plan, artifactImage(plan, "a"))
	name := original["actor-template.json"].(map[string]any)["metadata"].(map[string]any)["name"]
	originalDigest := original["build.json"].(map[string]any)["templateDigest"]
	variants := []map[string]any{}
	for _, change := range []func(map[string]any){
		func(p map[string]any) { p["target"] = "coding" },
		func(p map[string]any) { p["atespace"] = "other" },
		func(p map[string]any) {
			p["network"] = map[string]any{"tls": map[string]any{"hosts": []any{"github.com"}, "ports": []any{443}}}
		},
		func(p map[string]any) {
			p["actor"].(map[string]any)["containers"].([]any)[0].(map[string]any)["command"] = []any{"bash"}
		},
	} {
		changed := deepCopy(t, plan).(map[string]any)
		change(changed)
		variants = append(variants, mustRender(t, changed, artifactImage(changed, "a")))
	}
	variants = append(variants, mustRender(t, plan, artifactImage(plan, "b")))
	for _, result := range variants {
		if result["actor-template.json"].(map[string]any)["metadata"].(map[string]any)["name"] == name {
			t.Fatal("name must change for every runtime input")
		}
		if result["build.json"].(map[string]any)["templateDigest"] == originalDigest {
			t.Fatal("templateDigest must change for every runtime input")
		}
	}
	// Прямая проверка хэша канонического JSON.
	actor := deepCopy(t, original["actor-template.json"]).(map[string]any)
	delete(actor, "metadata")
	canonical, err := canonicalArtifactJSON(map[string]any{
		"actor": actor, "network": plan["network"],
		"identity": map[string]any{"project": plan["project"], "target": plan["target"],
			"environment": plan["environment"], "atespace": plan["atespace"]},
	})
	wantNoError(t, err)
	digest := sha256.Sum256([]byte(canonical))
	if original["build.json"].(map[string]any)["templateDigest"] != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatal("templateDigest must equal sha256 of canonical JSON")
	}
	// Порядок ключей плана не влияет на результат.
	reordered := map[string]any{}
	keys := make([]string, 0, len(plan))
	for key := range plan {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, key := range keys {
		reordered[key] = plan[key]
	}
	if marshalFor(mustRender(t, reordered, artifactImage(plan, "a"))) != marshalFor(original) {
		t.Fatal("render must not depend on key order")
	}
	// Длинная цель не ломает DNS-метку имени.
	long := deepCopy(t, plan).(map[string]any)
	long["target"] = strings.Repeat("a", 63)
	longName := mustRender(t, long, artifactImage(long, "a"))["actor-template.json"].(map[string]any)["metadata"].(map[string]any)["name"].(string)
	if len(longName) > 63 {
		t.Fatalf("name too long: %d", len(longName))
	}
}

func TestProvenanceWhitelistsReportsAndIncludesBuildInputs(t *testing.T) {
	plan := artifactPlan(t)
	plan["build"].(map[string]any)["arguments"] = map[string]any{"VERSION": "3.12"}
	plan["pins"] = map[string]any{"VERSION": "3.12"}
	reports := map[string]any{"project": map[string]any{
		"revision": strings.Repeat("c", 40), "repository": "https://github.com/team/repo",
		"path": "/machine/cache", "token": "private-value"}}
	result, err := Render(plan, artifactImage(plan, "a"), reports)
	wantNoError(t, err)
	provenance := result["build.json"].(map[string]any)
	if provenance["apiVersion"] != plan["apiVersion"] {
		t.Fatal("apiVersion")
	}
	if !reflectDeepEqual(provenance["build"], plan["build"]) {
		t.Fatal("build inputs")
	}
	if !reflectDeepEqual(provenance["pins"], plan["pins"]) {
		t.Fatal("pins")
	}
	if provenance["image"] != artifactImage(plan, "a") {
		t.Fatal("image")
	}
	if provenance["imageDigest"] != "sha256:"+strings.Repeat("a", 64) {
		t.Fatal("imageDigest")
	}
	source := provenance["sources"].(map[string]any)["project"].(map[string]any)
	if source["revision"] != strings.Repeat("c", 40) {
		t.Fatal("report revision")
	}
	if source["repository"] != "https://github.com/team/repo" {
		t.Fatal("report repository")
	}
	encoded := marshalFor(provenance)
	if strings.Contains(encoded, "/machine") || strings.Contains(encoded, "private-value") {
		t.Fatal("provenance leaks machine paths or report extras")
	}
	// Отчёт не разделяет память с результатом.
	reports["project"].(map[string]any)["revision"] = strings.Repeat("d", 40)
	if source["revision"] != strings.Repeat("c", 40) {
		t.Fatal("provenance must copy report values")
	}
	for _, report := range []map[string]any{
		{"revision": "bad"},
		{"repository": "https://user:private-value@github.com/repo"},
	} {
		_, err := Render(plan, artifactImage(plan, "a"), map[string]any{"project": any(report)})
		if err == nil {
			t.Fatalf("report %v must be refused", report)
		}
		if strings.Contains(err.Error(), "private-value") {
			t.Fatalf("error discloses report secrets: %v", err)
		}
	}
}

func TestDigestAndUintRefusals(t *testing.T) {
	plan := artifactPlan(t)
	reference := artifactImage(plan, "a")
	for _, value := range []string{"", strings.Replace(reference, "sha256:", "sha512:", 1),
		reference[:len(reference)-1], strings.Replace(reference, "/example/", "/other/", 1),
		strings.Replace(reference, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		reference + ":tag"} {
		if _, err := Render(plan, value, nil); err == nil {
			t.Errorf("image %q must be refused", value)
		}
	}
	for _, value := range []any{"0", "-1", "18446744073709551616", "oops", true} {
		changed := deepCopy(t, plan).(map[string]any)
		limits := changed["actor"].(map[string]any)["resources"].(map[string]any)["limits"].(map[string]any)
		limits["memoryBytes"] = value
		if _, err := Render(changed, artifactImage(changed, "a"), nil); err == nil {
			t.Errorf("resource %v must be refused", value)
		}
	}
}
