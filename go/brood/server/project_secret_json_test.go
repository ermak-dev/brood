package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// FEATURE: Secret requests reject ambiguous JSON without echoing submitted inputs.
// SPEC: Duplicate keys, nulls and trailing values never reach storage.
func TestProjectSecretJSONRejectsAmbiguity(t *testing.T) {
	for _, body := range []string{
		`{"vars":{"APP_TOKEN":"synthetic-first","APP_TOKEN":"synthetic-second"}}`,
		`{"vars":{"APP_TOKEN":null}}`,
		`{"vars":null}`,
		`{"vars":{}} {"vars":{}}`,
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("PUT", "/", strings.NewReader(body))
		var value struct {
			Vars map[string]string `json:"vars"`
		}
		if readProjectSecretBody(response, request, &value) {
			t.Fatal("ambiguous secret request accepted")
		}
		if strings.Contains(response.Body.String(), "synthetic-") {
			t.Fatal("secret error echoed input")
		}
	}
}
