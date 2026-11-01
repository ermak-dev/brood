package server

import (
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExternalTLSProxyUsesBackendAuthority(t *testing.T) {
	var authority string
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != authority {
			t.Errorf("external TLS backend received the caller's authority instead of its own")
		}
		io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	authority = strings.TrimPrefix(backend.URL, "https://")
	trusted := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})
	proxy := llmProxy(Config{CLIProxyURL: backend.URL, CLIProxyCAPEM: trusted}, false)
	request := httptest.NewRequest("GET", "https://brood.invalid/v1/models", nil)
	proxy.ServeHTTP(httptest.NewRecorder(), request)
}

func TestManagedLLMUsesExplicitProxyCAAndRejectsWrongCA(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":[]}`) }))
	defer backend.Close()
	trusted := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})
	_, wrong := selfSignFake(t, "wrong-proxy-ca")
	for _, tc := range []struct {
		name   string
		ca     []byte
		status int
	}{{"untrusted", nil, 502}, {"trusted", trusted, 200}, {"wrong", wrong, 502}} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _, _, base, token := managedLLMSetup(t, backend.URL, "synthetic-key", tc.ca)
			response := llmRequest(t, ts.srv.Handler(), "GET", base+"/v1/models", token, nil)
			if response.Code != tc.status {
				t.Fatalf("status %d want %d", response.Code, tc.status)
			}
		})
	}
	cfg, err := LoadConfig(func(key string) string {
		if key == "CLIPROXY_CA_PEM_B64" {
			return base64.StdEncoding.EncodeToString(trusted)
		}
		return ""
	})
	if err != nil || string(cfg.CLIProxyCAPEM) != string(trusted) {
		t.Fatalf("CA configuration: %v", err)
	}
	_, err = LoadConfig(func(key string) string {
		if key == "CLIPROXY_CA_PEM_B64" {
			return "%%%"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "CLIPROXY_CA_PEM_B64") {
		t.Fatalf("bad encoding accepted: %v", err)
	}
}
