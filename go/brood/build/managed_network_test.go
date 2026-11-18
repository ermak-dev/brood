package build

import (
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/egress"
)

func TestProjectManagedNetworkPreservesSelectedRules(t *testing.T) {
	document := minimal(t)
	document["project"] = map[string]any{"managed_network": "tools"}
	networks, _ := asMap(document["networks"])
	networks["tools"] = map[string]any{
		"http": map[string]any{"hosts": []any{"Tool.Example.COM"}, "ports": []any{8080}},
		"tls":  map[string]any{"hosts": []any{"git.example.com"}, "ports": []any{443, 8443}},
	}
	plan := planned(t, document, planOptions{})
	declaration := plan["projectConfig"].(map[string]any)
	raw, err := json.Marshal(declaration["managed_network"])
	if err != nil {
		t.Fatal(err)
	}
	policy, err := egress.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Allows(egress.HTTP, "tool.example.com", 8080) || !policy.Allows(egress.TLS, "git.example.com", 443) || !policy.Allows(egress.TLS, "git.example.com", 8443) || policy.Allows(egress.TLS, "tool.example.com", 8080) || policy.Allows(egress.HTTP, "git.example.com", 443) {
		t.Fatal("planner flattened or broadened selected network")
	}
	networks["tools"].(map[string]any)["http"].(map[string]any)["hosts"].([]any)[0] = "changed.example"
	after, err := json.Marshal(declaration["managed_network"])
	if err != nil || string(after) != string(raw) {
		t.Fatal("input rewrote project plan")
	}
	if runtimeNetwork, _ := asMap(plan["network"]); len(runtimeNetwork) != 0 {
		t.Fatal("managed project setting changed the selected native template network")
	}
}

func TestProjectManagedNetworkOmissionClearAndOfflineDiffer(t *testing.T) {
	plan := planned(t, nil, planOptions{})
	declaration := plan["projectConfig"].(map[string]any)
	if _, present := declaration["managed_network"]; present {
		t.Fatal("omission synthesized a network declaration")
	}
	document := minimal(t)
	document["project"] = map[string]any{"managed_network": ""}
	plan = planned(t, document, planOptions{})
	declaration = plan["projectConfig"].(map[string]any)
	if value, present := declaration["managed_network"]; !present || value != nil {
		t.Fatal("explicit clear not represented by null")
	}
	document["project"].(map[string]any)["managed_network"] = "offline"
	plan = planned(t, document, planOptions{})
	declaration = plan["projectConfig"].(map[string]any)
	raw, err := json.Marshal(declaration["managed_network"])
	if err != nil {
		t.Fatal(err)
	}
	policy, err := egress.Decode(raw)
	if err != nil || policy.AllowsDNS("tool.example.com") {
		t.Fatalf("offline policy lost: %v", err)
	}
}

func TestProjectManagedNetworkRejectsUnknownReference(t *testing.T) {
	document := minimal(t)
	document["project"] = map[string]any{"managed_network": "missing"}
	_, err := MakePlan(document, nil, "", "localhost:5000/team", "development", "Broodfile")
	if err == nil || !strings.Contains(err.Error(), "project.managed_network") {
		t.Fatalf("missing reference: %v", err)
	}
}
