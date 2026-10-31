package relay

// Guest transport readiness does not establish ttyd application readiness.
// Both authentication layers must bind the current durable actor incarnation.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

const testToken = "private-guest-token"

type fixture struct {
	h       *Hub
	server  *httptest.Server
	cfg     ClientConfig
	current atomic.Bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.current.Store(true)
	var err error
	f.h, err = NewHub([]byte(strings.Repeat("k", 32)), func(uid string, g int64, token string) bool {
		return f.current.Load() && uid == "actor-uid" && g == 3 && token == testToken
	})
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewTLSServer(f.h.Handler())
	f.cfg = ClientConfig{URL: "wss" + strings.TrimPrefix(f.server.URL, "https"), ActorUID: "actor-uid", Generation: 3, Token: testToken, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), HostKey: f.h.HostKey(), DialTarget: func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { defer b.Close(); io.Copy(b, b) }()
		return a, nil
	}}
	t.Cleanup(func() { f.h.Close(); f.server.Close() })
	return f
}
func await(t *testing.T, p func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !p() {
		select {
		case <-ctx.Done():
			t.Fatal("condition not reached")
		default:
			runtime.Gosched()
		}
	}
}
func start(t *testing.T, f *fixture, cfg ClientConfig) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunClient(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client failed to stop")
		}
	})
	await(t, func() bool { return f.h.Connected("actor-uid", 3) })
	return cancel
}
func dial(t *testing.T, f *fixture) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, err := f.h.Dial(ctx, "actor-uid", 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func TestRelayRoundTripAndReplacement(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	c := dial(t, f)
	payload := strings.Repeat("raw\x00stream", 20000)
	go func() { io.WriteString(c, payload) }()
	got := make([]byte, len(payload))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != payload {
		t.Fatalf("roundtrip: %v", err)
	}
	old := f.h.session("actor-uid", 3)
	start(t, f, f.cfg)
	await(t, func() bool { return f.h.session("actor-uid", 3) != old })
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("replacement retained old stream")
	}
	c2 := dial(t, f)
	io.WriteString(c2, "new")
	b := make([]byte, 3)
	if _, err := io.ReadFull(c2, b); err != nil || string(b) != "new" {
		t.Fatal("replacement unavailable", err)
	}
	f.current.Store(false)
	if f.h.Connected("actor-uid", 3) {
		t.Fatal("revoked guest connected")
	}
	if _, err := f.h.Dial(context.Background(), "actor-uid", 3); err == nil {
		t.Fatal("revoked dial allowed")
	}
	c2.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c2.Read(b); err == nil {
		t.Fatal("revoked stream survived")
	}
}
func TestAuthBeforeUpgrade(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		path, auth string
		status     int
	}{{"/api/proxmox/guest/actor-uid/3", "", 401}, {"/api/proxmox/guest/actor-uid/2", "Bearer " + testToken, 401}, {"/api/proxmox/guest/other/3", "Bearer " + testToken, 401}, {"/api/proxmox/guest/actor-uid/03", "Bearer " + testToken, 404}, {"/api/proxmox/guest/actor-uid/3?token=" + testToken, "Bearer " + testToken, 404}} {
		req, _ := http.NewRequest("GET", f.server.URL+tc.path, nil)
		req.Header.Set("Authorization", tc.auth)
		req.AddCookie(&http.Cookie{Name: "session", Value: testToken})
		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("status %d want %d", resp.StatusCode, tc.status)
		}
	}
}
func untrustedCA(t *testing.T) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(104), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func TestTLSAndPinFailures(t *testing.T) {
	for _, name := range []string{"ca", "badpem", "hostname", "hostkey"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			cfg := f.cfg
			switch name {
			case "ca":
				cfg.CAPEM = untrustedCA(t)
			case "badpem":
				cfg.CAPEM = []byte("bad CA")
			case "hostname":
				cfg.URL = strings.Replace(cfg.URL, "127.0.0.1", "localhost", 1)
			case "hostkey":
				h, _ := NewHub([]byte(strings.Repeat("z", 32)), func(string, int64, string) bool { return true })
				defer h.Close()
				cfg.HostKey = h.HostKey()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			err := RunClient(ctx, cfg)
			if err == nil {
				t.Fatal("invalid trust accepted")
			}
			if f.h.Connected("actor-uid", 3) {
				t.Fatal("invalid trust registered")
			}
		})
	}
}
func TestConfigSecretsAndValidation(t *testing.T) {
	f := setup(t)
	for _, text := range []string{fmt.Sprint(f.cfg), fmt.Sprintf("%+v", f.cfg), fmt.Sprintf("%#v", f.cfg)} {
		if strings.Contains(text, testToken) {
			t.Fatal("fmt leaked token")
		}
	}
	b, err := json.Marshal(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testToken) {
		t.Fatal("JSON leaked token")
	}
	h, err := NewHub(nil, nil)
	if err == nil || h != nil {
		t.Fatal("empty hub settings accepted")
	}
	h, err = NewHub([]byte(strings.Repeat("k", 32)), func(string, int64, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.HostKey() != f.h.HostKey() {
		t.Fatal("host signer not deterministic")
	}
	for _, url := range []string{"ws://localhost", "wss://user:password@localhost", "wss://localhost/?token=" + testToken} {
		cfg := f.cfg
		cfg.URL = url
		err := RunClient(context.Background(), cfg)
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatal("invalid URL accepted or leaked")
		}
	}
}
func TestStreamBoundsCancelAndDeadline(t *testing.T) {
	f := setup(t)
	cancel := start(t, f, f.cfg)
	var conns []net.Conn
	for i := 0; i < MaxStreams; i++ {
		conns = append(conns, dial(t, f))
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := f.h.Dial(ctx, "actor-uid", 3); err == nil {
		t.Fatal("stream bound ignored")
	}
	c := conns[0]
	c.SetReadDeadline(time.Now().Add(-time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("deadline ignored")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := f.h.Dial(ctx2, "actor-uid", 3); err == nil {
		t.Fatal("canceled dial accepted")
	}
	cancel()
	await(t, func() bool { return !f.h.Connected("actor-uid", 3) })
}
func TestHTTPOverRelay(t *testing.T) {
	f := setup(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal" {
			t.Error("wrong path")
		}
		io.WriteString(w, "ttyd-ready")
	}))
	defer target.Close()
	f.cfg.DialTarget = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(target.URL, "http://"))
	}
	start(t, f, f.cfg)
	c := dial(t, f)
	io.WriteString(c, "GET /terminal HTTP/1.1\r\nHost: ttyd\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ttyd-ready" {
		t.Fatalf("body %q", b)
	}
}
func rawSSH(t *testing.T, f *fixture, password string) (ssh.Conn, <-chan ssh.NewChannel) {
	t.Helper()
	return rawSSHUser(t, f, "actor-uid/3", password)
}
func rawSSHUser(t *testing.T, f *fixture, user, password string) (ssh.Conn, <-chan ssh.NewChannel) {
	t.Helper()
	wc, _ := websocket.NewConfig(f.cfg.URL+"/api/proxmox/guest/actor-uid/3", f.server.URL)
	wc.TlsConfig = f.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	wc.Header.Set("Authorization", "Bearer "+testToken)
	ws, err := wc.DialContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws.PayloadType = websocket.BinaryFrame
	c, ch, req, err := ssh.NewClientConn(ws, "relay", &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(password)}, HostKeyCallback: ssh.FixedHostKey(mustKey(t, f.h.HostKey()))})
	if err != nil {
		ws.Close()
		return nil, nil
	}
	go ssh.DiscardRequests(req)
	t.Cleanup(func() { c.Close() })
	return c, ch
}
func mustKey(t *testing.T, s string) ssh.PublicKey {
	t.Helper()
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return k
}
func TestSSHAuthAndUnknownChannels(t *testing.T) {
	f := setup(t)
	for _, user := range []string{"actor-uid/2", "other/3"} {
		if c, _ := rawSSHUser(t, f, user, testToken); c != nil {
			c.Close()
			t.Fatal("SSH identity not bound to bearer endpoint")
		}
	}
	if c, _ := rawSSH(t, f, "wrong"); c != nil {
		t.Fatal("SSH password not bound to bearer")
	}
	c, _ := rawSSH(t, f, testToken)
	if c == nil {
		t.Fatal("SSH rejected valid guest")
	}
	if _, _, err := c.OpenChannel("direct-tcpip", nil); err == nil {
		t.Fatal("guest arbitrary forwarding allowed")
	}
	if ok, _, err := c.SendRequest("exec", true, nil); err != nil || ok {
		t.Fatal("global request accepted", err)
	}
}
