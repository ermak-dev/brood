package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG: encoding/json accepts case aliases after the exact duplicate-key scan.
// SPEC: Schema fields have exact names; variable-map keys remain case-sensitive.
func TestProjectSecretJSONRejectsCaseAliases(t *testing.T) {
	for _, body := range []string{
		`{"vars":{"APP_TOKEN":"synthetic-first"},"Vars":{"APP_TOKEN":"synthetic-second"}}`,
		`{"Vars":{"APP_TOKEN":"synthetic-first"}}`,
		`{"files":[{"name":"intended","Name":"other","data":"eA=="}]}`,
		`{"files":[{"name":"intended","Data":"eA=="}]}`,
		`{"Names":["APP_TOKEN"]}`,
	} {
		var value struct {
			Vars  map[string]string `json:"vars"`
			Files []struct {
				Name string  `json:"name"`
				Data *[]byte `json:"data"`
			} `json:"files"`
			Names []string `json:"names"`
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/", strings.NewReader(body))
		if readProjectSecretBody(w, r, &value) {
			t.Fatal("case-aliased schema field accepted")
		}
		if strings.Contains(w.Body.String(), "synthetic-") {
			t.Fatal("error echoed secret")
		}
	}
	var value struct {
		Vars map[string]string `json:"vars"`
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/", strings.NewReader(`{"vars":{"APP_TOKEN":"one","app_token":"two"}}`))
	if !readProjectSecretBody(w, r, &value) || len(value.Vars) != 2 {
		t.Fatal("distinct variable names were rejected")
	}
}
