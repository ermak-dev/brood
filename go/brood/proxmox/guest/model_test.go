//go:build linux

package guest

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestGuestModelGatewayPinsOriginAndKeepsMachineTokenOutOfActor(t *testing.T) {
	var calls atomic.Int32
	token := strings.Repeat("x", 43)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/proxmox/llm/actor/1/v1/messages/count_tokens" {
			t.Errorf("wrong route %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Cookie") != "" || r.Header.Get("X-Management-Key") != "" {
			t.Error("credential isolation failed")
		}
		w.Header().Set("Set-Cookie", "forbidden=value")
		io.WriteString(w, `{"input_tokens":12}`)
	}))
	defer backend.Close()
	handler, err := NewModelGateway(relay.ClientConfig{URL: "wss" + strings.TrimPrefix(backend.URL, "https") + "/api/proxmox/guest/actor/1", ActorUID: "actor", Generation: 1, Token: token, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{}`))
	request.Host = "127.0.0.1:8317"
	request.Header.Set("Authorization", "Bearer actor-supplied")
	request.Header.Set("Cookie", "session=forbidden")
	request.Header.Set("X-Management-Key", "forbidden")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Set-Cookie") != "" || !strings.Contains(response.Body.String(), `"input_tokens":12`) {
		t.Fatalf("response: %d %s", response.Code, response.Body)
	}
	for _, path := range []string{"/v8/management", "/v1/models?url=evil", "/v1/%6dodels"} {
		response = httptest.NewRecorder()
		request = httptest.NewRequest("GET", path, nil)
		request.Host = "127.0.0.1:8317"
		handler.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Errorf("forbidden path %s returned %d", path, response.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("forbidden route reached upstream")
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:8317", "ANTHROPIC_AUTH_TOKEN=brood-local", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1", "NO_PROXY=127.0.0.1,localhost"} {
		if !slices.Contains(actorEnv(), want) {
			t.Errorf("missing actor gateway setting %s", want)
		}
	}
	if strings.Contains(strings.Join(actorEnv(), "\n"), token) {
		t.Fatal("machine token leaked into actor environment")
	}
	if !strings.Contains(nftRules(), `tcp dport { 3128, 5353, 8317, 8318 } accept`) {
		t.Fatal("model gateway missing from fixed loopback policy")
	}
}
