package server

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestActorModelSourceDoesNotReplaceInstalledProxy(t *testing.T) {
	var controlCalls, actorCalls atomic.Int32
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		controlCalls.Add(1)
		io.WriteString(w, `{"data":[{"id":"control-source"}]}`)
	}))
	defer control.Close()
	actor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actorCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer actor-source-key" {
			t.Error("wrong actor-source credential")
		}
		io.WriteString(w, `{"data":[{"id":"actor-source"}]}`)
	}))
	defer actor.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: actor.Certificate().Raw})
	ts, _, _, base, token := managedLLMSetup(t, control.URL, "control-source-key")
	ts.srv.managedLLM = ts.srv.managedLLMHandler(Config{CLIProxyURL: control.URL, CLIProxyAPIKey: "control-source-key", ActorLLMURL: actor.URL, ActorLLMAPIKey: "actor-source-key", ActorLLMCAPEM: ca})
	response := llmRequest(t, ts.srv.Handler(), "GET", base+"/v1/models", token, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "actor-source") || controlCalls.Load() != 0 {
		t.Fatalf("actor route: %d %s", response.Code, response.Body)
	}
	response = llmRequest(t, ts.srv.Handler(), "GET", "/v1/models", "client-key", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "control-source") || controlCalls.Load() != 1 || actorCalls.Load() != 1 {
		t.Fatal("installed proxy routing was replaced")
	}
	values := map[string]string{"BROOD_ACTOR_LLM_URL": actor.URL, "BROOD_ACTOR_LLM_API_KEY": "actor-source-key", "BROOD_ACTOR_LLM_CA_PEM_B64": base64.StdEncoding.EncodeToString(ca)}
	cfg, err := LoadConfig(func(k string) string { return values[k] })
	if err != nil || cfg.ActorLLMURL != actor.URL || cfg.ActorLLMAPIKey != "actor-source-key" {
		t.Fatalf("actor source config: %v", err)
	}
	data, _ := json.Marshal(cfg)
	if strings.Contains(string(data), "actor-source-key") {
		t.Fatal("actor source credential leaked")
	}
	for _, change := range []map[string]string{{"BROOD_ACTOR_LLM_URL": "http://insecure.invalid", "BROOD_ACTOR_LLM_API_KEY": "key"}, {"BROOD_ACTOR_LLM_URL": actor.URL}, {"BROOD_ACTOR_LLM_API_KEY": "key"}} {
		if _, err := LoadConfig(func(k string) string { return change[k] }); err == nil {
			t.Fatal("incomplete or insecure actor source accepted")
		}
	}
}
