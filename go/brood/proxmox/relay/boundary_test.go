package relay

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

func TestClosedConnDiscardsBufferedBytes(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	c := dial(t, f).(*channelConn)
	c.readMu.Lock()
	c.buffer = []byte("already buffered")
	c.readMu.Unlock()
	c.Close()
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("closed stream exposed buffered data")
	}
}

func TestDisconnectClosesIdleConnResources(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	c := dial(t, f)
	f.h.Disconnect("actor-uid", 3)
	select {
	case <-c.(*channelConn).done:
	case <-time.After(time.Second):
		t.Fatal("disconnected stream retained idle adapter resources")
	}
}

func TestCanceledPendingChannelClosesPeer(t *testing.T) {
	f := setup(t)
	c, incoming := rawSSH(t, f, testToken)
	if c == nil {
		t.Fatal("SSH connection failed")
	}
	await(t, func() bool { return f.h.Connected("actor-uid", 3) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := f.h.Dial(ctx, "actor-uid", 3); done <- err }()
	select {
	case <-incoming:
	case <-time.After(3 * time.Second):
		t.Fatal("no channel request")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("dial result %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Wait() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled channel left SSH open and opening goroutine stranded")
	}
}
func TestGuestRejectsUnknownChannelsAndExtraData(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	p := f.h.session("actor-uid", 3)
	for _, tc := range []struct {
		kind  string
		extra []byte
	}{{"session", nil}, {"direct-tcpip", ssh.Marshal(struct{ Host string }{"169.254.169.254"})}, {terminalChannel, []byte("127.0.0.1:22")}} {
		if ch, _, err := p.conn.OpenChannel(tc.kind, tc.extra); err == nil {
			ch.Close()
			t.Fatal("guest accepted remote channel settings")
		}
	}
	if ok, _, err := p.conn.SendRequest("tcpip-forward", true, nil); err != nil || ok {
		t.Fatal("guest accepted global request", err)
	}
}
func TestDeadlineResetAndContextCancel(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := f.h.Dial(ctx, "actor-uid", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(-time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, context.DeadlineExceeded) {
		if e, ok := err.(net.Error); !ok || !e.Timeout() {
			t.Fatalf("not a timeout: %v", err)
		}
	}
	c.SetReadDeadline(time.Time{})
	io.WriteString(c, "a")
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != 'a' {
		t.Fatal("read deadline did not reset", err)
	}
	readDone := make(chan error, 1)
	go func() { _, err := c.Read(b); readDone <- err }()
	cancel()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("cancellation did not interrupt read")
		}
	case <-time.After(time.Second):
		t.Fatal("read not canceled")
	}
}
func TestHalfClose(t *testing.T) {
	f := setup(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		body, err := io.ReadAll(c)
		if err == nil {
			_, err = c.Write(append([]byte("reply:"), body...))
		}
		done <- err
	}()
	f.cfg.DialTarget = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
	start(t, f, f.cfg)
	c := dial(t, f)
	io.WriteString(c, "request")
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(c)
	if err != nil || string(body) != "reply:request" {
		t.Fatalf("half-close response %q %v", body, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestWebSocketOverTerminalStream(t *testing.T) {
	f := setup(t)
	target := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) { defer ws.Close(); ws.PayloadType = websocket.BinaryFrame; io.Copy(ws, ws) }))
	defer target.Close()
	f.cfg.DialTarget = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(target.URL, "http://"))
	}
	start(t, f, f.cfg)
	wc, _ := websocket.NewConfig("ws://ttyd/terminal", "http://panel")
	ws, err := websocket.NewClient(wc, dial(t, f))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := websocket.Message.Send(ws, []byte("browser-terminal")); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err := websocket.Message.Receive(ws, &body); err != nil || string(body) != "browser-terminal" {
		t.Fatal("WS roundtrip failed", err)
	}
}
func TestRevocationClosesIdleStreams(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	c := dial(t, f)
	f.current.Store(false)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("revoked stream survived")
	}
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("revocation only ended by read deadline")
	}
}
func TestCloseAndDisconnect(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	c := dial(t, f)
	f.h.Disconnect("actor-uid", 2)
	io.WriteString(c, "a")
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal("wrong generation disconnected", err)
	}
	f.current.Store(false)
	f.h.Disconnect("actor-uid", 3)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(b); err == nil {
		t.Fatal("disconnect retained stream")
	}
	f.h.Close()
	if f.h.Connected("actor-uid", 3) {
		t.Fatal("closed hub connected")
	}
}
func TestNoRedirect(t *testing.T) {
	f := setup(t)
	called := make(chan struct{}, 1)
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called <- struct{}{} }))
	defer other.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	cfg := f.cfg
	cfg.URL = "wss" + strings.TrimPrefix(redirect.URL, "https")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := RunClient(ctx, cfg); err == nil {
		t.Fatal("redirect accepted")
	}
	select {
	case <-called:
		t.Fatal("followed redirect")
	default:
	}
}
func TestAuthRecheckedDuringSSH(t *testing.T) {
	f := setup(t)
	wc, _ := websocket.NewConfig(f.cfg.URL+"/api/proxmox/guest/actor-uid/3", f.server.URL)
	wc.TlsConfig = f.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	wc.Header.Set("Authorization", "Bearer "+testToken)
	ws, err := wc.DialContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.PayloadType = websocket.BinaryFrame
	f.current.Store(false)
	c, _, _, err := ssh.NewClientConn(ws, "relay", &ssh.ClientConfig{User: "actor-uid/3", Auth: []ssh.AuthMethod{ssh.Password(testToken)}, HostKeyCallback: ssh.FixedHostKey(mustKey(t, f.h.HostKey()))})
	if err == nil {
		c.Close()
		t.Fatal("SSH did not recheck current database state")
	}
}
func TestClosedHubRejectsHandshake(t *testing.T) {
	f := setup(t)
	f.h.Close()
	req, _ := http.NewRequest("GET", f.server.URL+route(identity{"actor-uid", 3}), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(bufio.NewReader(resp.Body))
		t.Fatalf("closed status %d: %s", resp.StatusCode, b)
	}
}
