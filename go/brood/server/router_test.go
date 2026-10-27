package server

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func writeIdentity(t *testing.T, cert tls.Certificate, certFile, keyFile string) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAssembledRouterMTLS(t *testing.T) {
	routerCert, routerPEM := selfSignFake(t, "router.test")
	clientCert, clientPEM := selfSignFake(t, "client.test")
	rotatedCert, rotatedPEM := selfSignFake(t, "rotated.test")
	clients := x509.NewCertPool()
	clients.AppendCertsFromPEM(clientPEM)
	clients.AppendCertsFromPEM(rotatedPEM)
	var calls atomic.Int32
	var wantTarget string
	seen := make(chan string, 10)
	router := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			t.Error("router has no client identity")
		}
		if got := r.Header.Values("Ate-Target-Actor"); len(got) != 1 || got[0] != wantTarget {
			t.Errorf("target=%v", got)
		}
		for _, name := range []string{"Authorization", "X-CSRFToken"} {
			if r.Header.Get(name) != "" {
				t.Errorf("leaked %s", name)
			}
		}
		for _, c := range r.Cookies() {
			if c.Name == "session" || c.Name == "csrf" {
				t.Errorf("leaked cookie %s", c.Name)
			}
		}
		seen <- r.TLS.PeerCertificates[0].Subject.CommonName
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			rw.Flush()
			io.Copy(conn, rw)
			return
		}
		io.WriteString(w, "actor response")
	}))
	router.TLS = &tls.Config{Certificates: []tls.Certificate{routerCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clients}
	router.StartTLS()
	defer router.Close()
	ca := writeFileFake(t, "router-ca.pem", string(routerPEM))
	certFile := writeFileFake(t, "client.pem", "")
	keyFile := writeFileFake(t, "client-key.pem", "")
	writeIdentity(t, clientCert, certFile, keyFile)
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake, func(cfg *Config) {
		cfg.AtenetRouter = strings.TrimPrefix(router.URL, "https://")
		cfg.AtenetCA = ca
		cfg.AtenetCert = certFile
		cfg.AtenetKey = keyFile
		cfg.AtenetServerName = "router.test"
	})
	admin, alice, bob, swarmID := actorsSetup(t, ts.testServer)
	scope := swarmAtespace(t, ts.srv, swarmID)
	wantTarget = scope + "/sleeping"
	fake.mu.Lock()
	fake.actors[scope+"/sleeping"] = &ateapi.Actor{Metadata: fakeMeta("sleeping", scope)}
	fake.mu.Unlock()
	path := ts.http.URL + "/api/swarms/" + swarmID + "/proxy/actors/sleeping/echo"
	request := func(client *http.Client, upgrade bool, origin string, csrf bool) int {
		t.Helper()
		req, _ := http.NewRequest("GET", path, nil)
		req.Header.Set("Ate-Target-Actor", "foreign/spy")
		req.Header.Set("Authorization", "Bearer secret")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf {
			req.Header.Set("X-CSRFToken", ts.csrf(client))
		}
		if upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if got := request(bob, false, "", false); got != 404 {
		t.Fatalf("cross-user status=%d", got)
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached router")
	}
	if got := request(alice, false, "", false); got != 200 {
		t.Fatalf("suspended status=%d", got)
	}
	if got := <-seen; got != "client.test" {
		t.Fatalf("identity=%s", got)
	}
	writeIdentity(t, rotatedCert, certFile, keyFile)
	if got := request(alice, false, "", false); got != 200 {
		t.Fatalf("rotated status=%d", got)
	}
	if got := <-seen; got != "rotated.test" {
		t.Fatalf("stale identity=%s", got)
	}
	beforeFailure := calls.Load()
	if err := os.WriteFile(ca, clientPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if got := request(alice, false, "", false); got != 502 {
		t.Fatalf("untrusted router CA status=%d", got)
	}
	if calls.Load() != beforeFailure {
		t.Fatal("request reached untrusted router")
	}
	if err := os.WriteFile(ca, routerPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("invalid key"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := request(alice, false, "", false); got != 502 {
		t.Fatalf("invalid projected key status=%d", got)
	}
	writeIdentity(t, rotatedCert, certFile, keyFile)
	if got := request(alice, true, "https://evil.test", true); got != 403 {
		t.Fatalf("cross-origin websocket=%d", got)
	}
	if got := request(alice, true, ts.http.URL, false); got != 403 {
		t.Fatalf("websocket without CSRF=%d", got)
	}
	req, _ := http.NewRequest("GET", path+"?csrf="+ts.csrf(alice), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", ts.http.URL)
	for _, cookie := range alice.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	conn, err := net.Dial("tcp", req.URL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	handshake, err := http.ReadResponse(reader, req)
	if err != nil || handshake.StatusCode != 101 {
		t.Fatalf("TLS websocket handshake=%v err=%v", handshake, err)
	}
	<-seen
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("tunnel echo=%q err=%v", echo, err)
	}
	resp, data, _ := ts.do(alice, "POST", "/api/auth/logout", ts.csrf(alice), nil)
	expectStatus(t, "logout", resp, data, 204)
	ts.srv.proxy.RecheckNow()
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("websocket survived session revocation")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("websocket was not closed after logout")
	}
	ts.login(alice, "alice", "alice-password-1")
	before := calls.Load()
	resp, data, _ = ts.do(alice, "GET", "/api/swarms/"+swarmID+"/proxy/actors/missing", "", nil)
	expectStatus(t, "missing actor", resp, data, 404)
	if calls.Load() != before {
		t.Fatal("missing actor reached router")
	}
	// Потеря членства закрывает доступ владельцу до upstream.
	sw, _ := ts.srv.store.GetSwarm(nil2ctx(), swarmID)
	resp, data, _ = ts.do(admin, "DELETE", "/api/projects/"+sw.ProjectID+"/members/alice", ts.csrf(admin), nil)
	expectStatus(t, "remove owner membership", resp, data, 204)
	if got := request(alice, false, "", false); got != 404 {
		t.Fatalf("removed member=%d", got)
	}
	if calls.Load() != before {
		t.Fatal("removed member reached router")
	}
	// Router действительно требует сертификат, не только проверяет заголовки.
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(routerPEM)
	naked := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "router.test"}}}
	if resp, err := naked.Get(router.URL); err == nil {
		resp.Body.Close()
		t.Fatal("router accepted absent client certificate")
	}
}
