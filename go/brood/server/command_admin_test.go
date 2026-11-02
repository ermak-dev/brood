package server

// FEATURE: Authenticated administration commands without transport coupling.
// SPEC: Human admin for mutations/inventory, scoped project limits, redacted diagnostics.
// IMPLEMENTATION: Direct service calls, partial quota upsert, and private bounded model discovery.
// RESULT: IMPLEMENTED — command and provider tests pass with the parent server-field overlay.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

func adminCommandServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t).srv
	for _, u := range []store.User{{ID: "admin", Username: "admin", IsAdmin: true}, {ID: "ordinary", Username: "ordinary"}, {ID: "disabled", Username: "disabled", IsAdmin: true, Disabled: true}} {
		if err := s.store.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func runAdminCommand(s *Server, user, scope, name, args string) (command.Result, error) {
	return s.executeAdminCommand(context.Background(), user, scope, command.Invocation{Name: name, Arguments: json.RawMessage(args)})
}

func TestAdminCommandAuthorization(t *testing.T) {
	s := adminCommandServer(t)
	for _, user := range []string{"ordinary", "disabled", "missing"} {
		for _, name := range []string{"project_add", "project_limit", "node", "node_drain", "node_up", "server_diagnose"} {
			if _, err := runAdminCommand(s, user, "", name, `{}`); err == nil {
				t.Errorf("%s allowed %s", user, name)
			} else if user == "ordinary" && !strings.Contains(err.Error(), "administrator") {
				t.Errorf("%s rejected for wrong reason: %v", name, err)
			}
		}
	}
	for _, user := range []string{"ordinary", "admin"} {
		if _, err := runAdminCommand(s, user, "", "profile", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runAdminCommand(s, "disabled", "", "profile", `{}`); err == nil {
		t.Fatal("disabled profile allowed")
	}
}
func TestAdminCommandProjectAndLimit(t *testing.T) {
	s := adminCommandServer(t)
	result, err := runAdminCommand(s, "admin", "", "project_add", `{"name":"demo"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Brood project") {
		t.Fatalf("ambiguous project semantics: %s", result.Text)
	}
	p, found, err := s.catalog.ProjectByName(context.Background(), "demo")
	if err != nil || !found {
		t.Fatalf("project missing: %v", err)
	}
	n := int64(9)
	original := store.ProjectCapacity{WorkerSelector: map[string]string{"zone": "private"}, MaxSwarms: &n, MaxCPUMillicores: &n, MaxMemoryBytes: &n}
	if err := s.store.SetProjectCapacity(context.Background(), p.ID, original); err != nil {
		t.Fatal(err)
	}
	if _, err := runAdminCommand(s, "admin", "", "project_limit", `{"name":"demo","actors":0}`); err != nil {
		t.Fatal(err)
	}
	cap, err := s.store.GetProjectCapacity(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cap.MaxActors == nil || *cap.MaxActors != 0 || cap.MaxSwarms == nil || *cap.MaxSwarms != 9 || cap.MaxCPUMillicores == nil || *cap.MaxCPUMillicores != 9 || cap.MaxMemoryBytes == nil || *cap.MaxMemoryBytes != 9 || cap.WorkerSelector["zone"] != "private" {
		t.Fatalf("patch lost capacity: %+v", cap)
	}
	if _, err := runAdminCommand(s, "admin", "", "project_limit", `{"name":"demo","actors":-1}`); err == nil {
		t.Fatal("negative limit accepted")
	}
	if _, err := runAdminCommand(s, "admin", "", "project_limit", `{}`); err == nil {
		t.Fatal("missing project accepted outside scope")
	}
	if _, err := runAdminCommand(s, "admin", "foreign-swarm", "project_limit", `{"name":"demo","actors":4}`); err == nil {
		t.Fatal("foreign scope mutated project")
	}
	cap, _ = s.store.GetProjectCapacity(context.Background(), p.ID)
	if *cap.MaxActors != 0 {
		t.Fatal("denied call changed limit")
	}
}
func TestAdminCommandNodeQualifiers(t *testing.T) {
	s := adminCommandServer(t)
	for _, name := range []string{"pool", "other:pool", "managed:", "native:", "managed:../pool", "managed:pool:extra"} {
		args, _ := json.Marshal(map[string]string{"name": name})
		for _, op := range []string{"node_drain", "node_up"} {
			if _, err := runAdminCommand(s, "admin", "", op, string(args)); err == nil {
				t.Errorf("accepted %s %q", op, name)
			}
		}
	}
	if _, err := runAdminCommand(s, "admin", "", "node_up", `{"name":"native:worker"}`); err == nil || !strings.Contains(err.Error(), "undrain") {
		t.Fatalf("native undrain: %v", err)
	}
}
func TestAdminCommandDiagnosticAndProfile(t *testing.T) {
	s := adminCommandServer(t)
	result, err := runAdminCommand(s, "admin", "", "server_diagnose", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	text := string(result.Data) + result.Text
	for _, forbidden := range []string{"127.0.0.1", "password", "token", "secret", "brood.db"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("diagnostic contains %q: %s", forbidden, text)
		}
	}
	result, err = runAdminCommand(s, "ordinary", "", "profile", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var profile struct {
		Username string `json:"username"`
		Admin    bool   `json:"admin"`
	}
	if err := json.Unmarshal(result.Data, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Username != "ordinary" || profile.Admin {
		t.Fatalf("profile: %s", result.Data)
	}
	if _, err := runAdminCommand(s, "ordinary", "", "models", `{}`); err == nil || strings.Contains(err.Error(), "administrator") {
		t.Fatalf("model auth/config: %v", err)
	}
}
