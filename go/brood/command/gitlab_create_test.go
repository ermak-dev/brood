package command

import (
	"encoding/json"
	"strings"
	"testing"
)

// FEATURE: Типизированные команды MR create и чтения journal без authority.
// SPEC: Нет guest URL/project/target/credential; явный expected SHA и request key.
// IMPLEMENTATION: Только декларации команд, без server/store исполнения.
// RESULT: IMPLEMENTED — metadata, строгие поля и defaults проверены.
// STATUS: DONE
func TestGitLabCreateCatalog(t *testing.T) {
	s, ok := Lookup("gitlab_mr_create")
	if !ok || s.ReadOnly || !s.Guest || s.Destructive {
		t.Fatalf("wrong create metadata: %+v", s)
	}
	body := `{"request_id":"r1","source_branch":"brood/actor/1/topic","title":"Work","expected_source_sha":"` + strings.Repeat("a", 40) + `"}`
	inv, err := ParseTool("gitlab_mr_create", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if json.Unmarshal(inv.Arguments, &args) != nil || args["description"] != "" {
		t.Fatalf("description default: %s", inv.Arguments)
	}
	for _, field := range []string{"url", "project_id", "token", "target_branch", "actor_uid", "grant_id"} {
		bad := strings.TrimSuffix(body, "}") + `,"` + field + `":"forged"}`
		if _, err := ParseTool("gitlab_mr_create", []byte(bad)); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
	for _, bad := range []string{`{}`, strings.Replace(body, `"request_id":"r1",`, "", 1), strings.Replace(body, `"title":"Work"`, `"title":null`, 1), strings.Replace(body, `"request_id":"r1"`, `"request_id":"`+strings.Repeat("x", 129)+`"`, 1)} {
		if _, err := ParseTool("gitlab_mr_create", []byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	s, ok = Lookup("gitlab_operation_get")
	if !ok || !s.ReadOnly || !s.Guest || s.Destructive {
		t.Fatalf("wrong status metadata: %+v", s)
	}
	if _, err := ParseTool("gitlab_operation_get", []byte(`{"operation_id":"op"}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"operation_id":""}`, `{"operation_id":"op","project_id":"p"}`} {
		if _, err := ParseTool("gitlab_operation_get", []byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
