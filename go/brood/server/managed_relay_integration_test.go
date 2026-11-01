package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"golang.org/x/net/websocket"
)

// FEATURE: Existing isolated actor-view uses the real authenticated relay.
// SPEC: HTTP and upgrades share only the fixed terminal channel, preserve CSP,
// and close on durable desired-state revocation without native router fallback.
func TestManagedRealRelayActorViewHTTPUpgradeAndRevocation(t *testing.T) {
	ts, alice, _, id, pve, _ := managedSetup(t)
	pve.startUPID = "UPID:node:00000001:00000002:00000003:vzstart:105:user@pve!brood:"
	hub, err := relay.NewHub([]byte(ts.srv.managed.registry.relaySecret), ts.srv.managed.authorizeGuest)
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.managed.hub = hub
	terminal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:8080" || r.Header.Get("Origin") != "http://127.0.0.1:8080" && r.Header.Get("Origin") != "" {
			http.Error(w, "wrong terminal authority", 403)
			return
		}
		if r.URL.Path == "/ws" {
			websocket.Handler(func(c *websocket.Conn) { defer c.Close(); io.Copy(c, c) }).ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Set-Cookie", "danger=1")
		io.WriteString(w, "terminal")
	}))
	defer terminal.Close()
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "terminal"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	uid := b["metadata"].(map[string]any)["uid"].(string)
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	tlsServer := httptest.NewTLSServer(ts.srv.Handler())
	defer tlsServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	defer func() {
		cancel()
		hub.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("guest did not stop")
		}
	}()
	go func() {
		done <- relay.RunClient(ctx, relay.ClientConfig{URL: "wss" + strings.TrimPrefix(tlsServer.URL, "https"), ActorUID: uid, Generation: 1, Token: pve.spec.Env["BROOD_GUEST_RELAY_TOKEN"], CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}), HostKey: hub.HostKey(), DialTarget: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(terminal.URL, "http://"))
		}})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !hub.Connected(uid, 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !hub.Connected(uid, 1) {
		t.Fatal("guest did not authenticate")
	}
	ts.srv.managed.reconcileOnce(context.Background())
	// BUG: Истекший task log ломал adoption при живом owned CT и реальном relay.
	// HYPOTHESIS: Runtime повторно опрашивал завершённый UPID перед readiness.
	// SOLUTION: Stage=done заменяет повторный polling, но не ownership/relay/ttyd probe.
	// RESULT: FIXED — HTTP 404 старого task не мешает настоящему HTTPS/WSS/SSH adoption.
	// STATUS: FIXED
	// Проверяем настоящий HTTPS PVE client, WSS/SSH relay и HTTP ttyd вместе.
	a := mustManagedActor(t, ts, id)
	var taskReads, writes, configReads atomic.Int64
	pveHTTP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			w.WriteHeader(500)
			return
		}
		var data any
		switch {
		case strings.Contains(r.URL.Path, "/tasks/"):
			taskReads.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		case strings.HasSuffix(r.URL.Path, "/config"):
			configReads.Add(1)
			data = map[string]any{"description": proxmox.OwnedDescription(managedOwned(a).Owner), "unprivileged": 1, "rootfs": "disks:subvol-105-disk-0,size=4G", "digest": strings.Repeat("a", 40), "hostname": "terminal", "net0": "name=eth0,bridge=vmbr1,ip=dhcp,hwaddr=02:00:00:00:00:01,host-managed=1", "entrypoint": "/usr/bin/tini -- /usr/local/bin/brood-guest", "cmode": "console", "lxc": [][]string{{"lxc.init.cwd", "/workspace"}, {"lxc.signal.halt", "SIGTERM"}}}
		case r.URL.Path == "/api2/json/pools":
			data = []any{map[string]any{"poolid": "actors", "members": []any{map[string]any{"type": "lxc", "node": "node", "vmid": 105}}}}
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			data = map[string]any{"status": "running"}
		default:
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer pveHTTP.Close()
	pveClient, err := proxmox.NewClient(proxmox.Config{Endpoint: pveHTTP.URL, TokenID: "user@pve!brood", TokenSecret: "test-only-pve-token", CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pveHTTP.Certificate().Raw})), Node: "node", ResourcePool: "actors", Storage: "disks", Network: "vmbr1", TemplateStorage: "templates", TemplatePrefix: "brood-"})
	if err != nil {
		t.Fatal(err)
	}
	defer pveClient.Close()
	if err := pveClient.WaitTask(context.Background(), a.UPID); !errors.Is(err, proxmox.ErrNotFound) {
		t.Fatalf("fixture task log is not HTTP 404: %v", err)
	}
	ts.srv.managed.clients[a.PoolUID] = pveClient
	ts.srv.managed.verified.Delete(a.UID)
	ts.srv.managed.reconcileOnce(context.Background())
	adopted := mustManagedActor(t, ts, id)
	if adopted.Phase != "running" || adopted.Stage != "done" || taskReads.Load() != 1 || configReads.Load() != 3 || writes.Load() != 0 || adopted.Revision != a.Revision {
		t.Fatalf("expired-log adoption failed: phase=%s stage=%s taskReads=%d configReads=%d writes=%d", adopted.Phase, adopted.Stage, taskReads.Load(), configReads.Load(), writes.Load())
	}
	ts.srv.managed.clients[a.PoolUID] = pve
	client := *alice
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(ts.http.URL + "/api/swarms/" + id + "/proxy/actors/terminal/")
	if err != nil {
		t.Fatal(err)
	}
	location := response.Header.Get("Location")
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatalf("ticket status %d", response.StatusCode)
	}
	response, err = http.Get(ts.http.URL + location)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(data) != "terminal" || response.Header.Get("Set-Cookie") != "" || !strings.Contains(response.Header.Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("managed HTTP isolation failed: %d %q %#v", response.StatusCode, data, response.Header)
	}
	wsConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(ts.http.URL+location+"ws", "http"), ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.DialConfig(wsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := ws.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 5)
	if _, err := io.ReadFull(ws, echo); err != nil || string(echo) != "hello" {
		t.Fatalf("managed upgrade failed: %q %v", echo, err)
	}
	r, b, _ = ts.do(alice, "POST", path+"/terminal", ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "revoke desired state", r, b, 200)
	if _, err := ws.Read(echo); err == nil {
		t.Fatal("revoked browser stream remained open")
	}
}
