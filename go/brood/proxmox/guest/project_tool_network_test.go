//go:build linux

package guest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/proxmox/egress"
	"brood.dev/go/brood/proxmox/relay"
)

func TestProjectToolNetworkHelper(t *testing.T) {
	if os.Getenv("BROOD_TOOL_FIXTURE") != "1" {
		return
	}
	if os.Getenv("BROOD_GUEST_RELAY_TOKEN") != "" || os.Getenv("GITLAB_TOKEN") != "" {
		os.Exit(21)
	}
	ca, err := os.ReadFile(os.Getenv("TOOL_CA_PATH"))
	if err != nil {
		os.Exit(22)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		os.Exit(23)
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequest("GET", os.Getenv("TOOL_URL"), nil)
	if err != nil {
		os.Exit(24)
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("APP_TOKEN"))
	response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture HTTPS request:", err)
		os.Exit(25)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "project-tool-ok" || response.TLS == nil || len(response.TLS.VerifiedChains) == 0 {
		os.Exit(26)
	}
	os.Exit(0)
}

func TestProjectToolSecretsAndSelectedHTTPSNetwork(t *testing.T) {
	marker := "synthetic-project-only-value"
	tool := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+marker {
			t.Error("project tool did not receive its deliberate credential")
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, "project-tool-ok")
	}))
	defer tool.Close()
	toolCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tool.Certificate().Raw})
	bundle := projectsecret.Bundle{Revision: 7, Vars: map[string]string{"APP_TOKEN": marker}, Files: []projectsecret.File{{Name: "tool-ca.pem", Data: toolCA}}}
	supervisorToken := strings.Repeat("a", 43)
	secrets := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/proxmox/secrets/actor-fixture/3" || r.Header.Get("Authorization") != "Bearer "+supervisorToken {
			t.Error("supervisor secret fetch lost identity")
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(bundle)
	}))
	defer secrets.Close()
	cfg := relay.ClientConfig{URL: strings.Replace(secrets.URL, "https:", "wss:", 1) + "/api/proxmox/guest/actor-fixture/3", ActorUID: "actor-fixture", Generation: 3, Token: supervisorToken, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secrets.Certificate().Raw})}
	fetched, err := FetchProjectSecrets(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, home := t.TempDir(), t.TempDir()
	secretTestGit(t, root, "init", "-q")
	vars, err := applyProjectSecrets(root, home, fetched)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewNetworkPolicy([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{"example.com"}, Ports: []int{8443}}}, publicLookup)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewProxy(policy, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:8443" {
			t.Errorf("tool used unchecked target %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, tool.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	server := httptest.NewServer(proxy)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProjectToolNetworkHelper$")
	command.Dir = root
	command.Env = append(append([]string{}, vars...), "BROOD_TOOL_FIXTURE=1", "HOME="+home, "PATH="+os.Getenv("PATH"), "HTTP_PROXY="+server.URL, "HTTPS_PROXY="+server.URL, "NO_PROXY=", "TOOL_URL=https://example.com:8443/", "TOOL_CA_PATH="+filepath.Join(root, "tool-ca.pem"))
	if output, err := command.CombinedOutput(); err != nil {
		safe := strings.NewReplacer(marker, "[project value]", supervisorToken, "[supervisor value]").Replace(string(output))
		t.Fatalf("synthetic project tool failed: %v, output=%q", err, safe)
	}
}
