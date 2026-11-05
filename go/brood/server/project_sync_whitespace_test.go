package server

import "testing"

// FEATURE: Стандартные пробелы JSON в декларации проекта.
// SPEC: Пробелы вокруг документа и вложенных объектов не меняют контракт.
func TestProjectSyncJSONWhitespace(t *testing.T) {
	ts, admin, _, _, _ := gitRepositorySetup(t)
	w := syncRaw(t, ts, admin, " \n {\"name\":\"spaced\",\"repository\":\"\",\"capacity\": {\"worker_selector\": { } }} \n")
	if w.Code != 201 {
		t.Fatalf("valid whitespace: %d %s", w.Code, w.Body.String())
	}
}
