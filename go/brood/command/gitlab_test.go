package command

import "testing"

// FEATURE: Узкие read-only команды GitLab привязанного проекта.
// SPEC: Только положительный IID; URL, project и token не принимаются.
// IMPLEMENTATION: Декларации каталога без исполнения и authority.
// RESULT: IMPLEMENTED — проверки каталога и адаптеров проходят.
// STATUS: DONE
func TestGitLabReadCatalog(t *testing.T) {
	for name, arg := range map[string]string{"gitlab_issue_get": "iid", "gitlab_ci_get": "mr_iid"} {
		s, ok := Lookup(name)
		if !ok || !s.ReadOnly || !s.Guest || s.Destructive {
			t.Fatalf("missing read-only guest command %s", name)
		}
		for _, body := range []string{`{}`, `{"` + arg + `":0}`, `{"` + arg + `":-1}`, `{"` + arg + `":1.5}`, `{"` + arg + `":1,"project":42}`, `{"` + arg + `":1,"url":"https://foreign"}`, `{"` + arg + `":1,"token":"secret"}`} {
			if _, err := ParseTool(name, []byte(body)); err == nil {
				t.Fatalf("accepted %s %s", name, body)
			}
		}
		if _, err := ParseTool(name, []byte(`{"`+arg+`":7}`)); err != nil {
			t.Fatal(err)
		}
	}
}
