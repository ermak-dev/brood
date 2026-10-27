package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepositoryWorkbenchContract(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	document, err := ParseDocument(read("Broodfile"), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(read(document["pinsFile"].(string))), &pins); err != nil {
		t.Fatal(err)
	}
	plans := make([]map[string]any, 0, 2)
	for _, target := range []string{"development", "coding"} {
		plan, err := MakePlan(document, pins, target, "localhost:5002", "build-test", "Broodfile")
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plan)
		actor := plan["actor"].(map[string]any)
		limits := actor["resources"].(map[string]any)["limits"].(map[string]any)
		if !reflect.DeepEqual(limits, map[string]any{"cpuMillicores": "4000", "memoryBytes": "8589934592"}) {
			t.Fatalf("unexpected guest resources: %v", limits)
		}
		containers := actor["containers"].([]any)
		if len(containers) != 1 {
			t.Fatal("workbench must use one isolated guest container")
		}
		caps := containers[0].(map[string]any)["securityContext"].(map[string]any)["capabilities"].(map[string]any)["add"].([]any)
		if len(caps) != 41 {
			t.Fatalf("guest capability contract changed: %v", caps)
		}
		network := marshal(t, plan["network"])
		if strings.Contains(network, "*") {
			t.Fatal("workbench egress must use exact hosts")
		}
		if strings.Contains(network, "api.anthropic.com") != (target == "coding") {
			t.Fatal("model API access belongs only to the coding profile")
		}
	}
	for _, key := range []string{"build", "sources"} {
		if !reflect.DeepEqual(plans[0][key], plans[1][key]) {
			t.Fatalf("targets must share %s", key)
		}
	}
	if reflect.DeepEqual(plans[0]["actor"], plans[1]["actor"]) {
		t.Fatal("targets must retain distinct runtime commands")
	}
	revision := pins["SUBSTRATE_COMMIT"]
	if len(revision) != 40 || !strings.Contains(read("go/go.mod"), revision[:12]) {
		t.Fatal("Go dependency and Broodfile must share the pinned Substrate commit")
	}
	if !strings.Contains(pins["PYTHON_IMAGE"], "@sha256:") {
		t.Fatal("Python base image must be digest pinned")
	}
	for _, path := range []string{".ci/test.sh", "deploy/provision.sh"} {
		if !strings.Contains(read(path), "brood/dev/pins.json") {
			t.Fatalf("%s must use shared pins", path)
		}
	}
	for _, overlay := range []string{"COPY runtime/", "COPY workbench.sh"} {
		if strings.Contains(read("brood/dev/workbench/Dockerfile"), overlay) {
			t.Fatalf("runtime overlay remains: %s", overlay)
		}
	}
}
