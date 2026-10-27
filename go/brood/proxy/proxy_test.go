// Проверки прокси: гейт до соединения, стриппинг опасных заголовков,
// cookie, WebSocket, отзыв доступа на живом соединении, таймауты,
// отказы. Upstream — httptest; WebSocket — эхо на stdlib-хайджеке.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/app"
)

// testUpstream — записывающий upstream: статус, ответ, увиденные
// заголовки.
type testUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	hdr    http.Header
	body   []byte
	host   string
}

func newTestUpstream(t *testing.T) *testUpstream {
	t.Helper()
	u := &testUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hdr = r.Header.Clone()
		u.body = body
		u.host = r.Host
		u.mu.Unlock()
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("upstream ok"))
	})
	u.server = httptest.NewServer(mux)
	t.Cleanup(u.server.Close)
	return u
}

func (u *testUpstream) base(t *testing.T) *url.URL {
	t.Helper()
	parsed, err := url.Parse(u.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func (u *testUpstream) snapshot() (http.Header, string, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hdr.Clone(), string(u.body), u.host
}

// harness — собранный прокси-сервер с фейковыми зависимостями.
type harness struct {
	authz  *fakeAuthz
	res    *fakeResolver
	audit  *auditLog
	proxy  *Proxy
	server *httptest.Server
}

func newHarness(t *testing.T, user CurrentUser, swarms fakeSwarms) *harness {
	t.Helper()
	h := &harness{
		authz: &fakeAuthz{allowed: map[string]bool{}},
		res:   &fakeResolver{},
		audit: &auditLog{},
	}
	h.proxy = New(Config{
		Swarms:          swarms,
		Authorize:       h.authz.Can,
		CurrentUser:     user,
		Upstream:        h.res,
		Audit:           h.audit.record,
		RecheckInterval: time.Hour, // вручную управляем в тестах отзыва
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	mux := http.NewServeMux()
	h.proxy.Mount(mux)
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
	return h
}

func (h *harness) allow(userIDs ...string) {
	for _, id := range userIDs {
		h.authz.set(id, true)
	}
}

// do — запрос через прокси; URI вида /swarms/{id}/actors/{name}[/api/...].
func (h *harness) do(t *testing.T, method, uri string, header http.Header, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+uri, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// activeSwarm — рой в active с данным atespace.
func activeSwarm(id, atespace string) app.Swarm {
	return app.Swarm{ID: id, Name: id, AtespaceName: atespace, State: app.StateActive}
}

// targetHeader — заголовки, выбирающие цель на стороне upstream; их
// клиент присылать не имеет права.
func targetHeader() http.Header {
	return http.Header{
		"X-Actor":        {"client-actor"},
		"X-Atespace":     {"client-atespace"},
		"X-Actor-Target": {"actor=evil;atespace=evil"},
	}
}

// ─── гейт: отказ до резолва и до соединения ─────────────────────────────────

func TestAnonymousDenied(t *testing.T) {
	h := newHarness(t, anonymousUser(), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if h.res.callCount() != 0 {
		t.Fatal("resolver must not run for anonymous request")
	}
}

func TestDeniedUser404(t *testing.T) {
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.res.set("at1", "a1", newTestUpstream(t).base(t))
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", nil, nil)
	defer resp.Body.Close()
	// Чужой рой неотличим от отсутствующего: 404 без перечисления.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if h.res.callCount() != 0 {
		t.Fatalf("resolver ran %d times, want 0: отказ до резолва", h.res.callCount())
	}
	// Аудит фиксирует отказ.
	events := h.audit.snapshot()
	if len(events) != 1 || events[0].Allowed {
		t.Fatalf("audit = %+v, want one denied event", events)
	}
	if events[0].Username != "alice" || events[0].SwarmID != "sw1" || events[0].Actor != "a1" {
		t.Fatalf("audit event = %+v", events[0])
	}
}

func TestAuthzErrorIs500(t *testing.T) {
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.authz.setErr(errors.New("db down"))
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if h.res.callCount() != 0 {
		t.Fatal("resolver must not run on authz error")
	}
}

func TestSwarmNotFound(t *testing.T) {
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{})
	h.allow("u1")
	resp := h.do(t, "GET", "/swarms/nope/actors/a1/api/echo", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if h.res.callCount() != 0 {
		t.Fatal("resolver must not run for missing swarm")
	}
}

// wrongStates — все состояния, кроме active, запрещают терминал/прокси.
func wrongStates() []app.SwarmState {
	return []app.SwarmState{
		app.StateProvisioning, app.StateDeleting, app.StateError, app.StateDeleted,
	}
}

func TestNonActiveSwarmRejected(t *testing.T) {
	for _, state := range wrongStates() {
		t.Run(string(state), func(t *testing.T) {
			up := newTestUpstream(t)
			swarms := fakeSwarms{"sw1": activeSwarm("sw1", "at1")}
			sw := swarms["sw1"]
			sw.State = state
			swarms["sw1"] = sw
			h := newHarness(t, fixedUser("u1", "alice"), swarms)
			h.allow("u1")
			h.res.set("at1", "a1", up.base(t))
			resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", nil, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", resp.StatusCode)
			}
			if h.res.callCount() != 0 {
				t.Fatal("resolver must not run for non-active swarm")
			}
		})
	}
}

func TestAuthzActionAndResource(t *testing.T) {
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", newTestUpstream(t).base(t))
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", nil, nil)
	defer resp.Body.Close()
	act, res := h.authz.lastAction()
	if act != app.ActionManageSwarm {
		t.Fatalf("action = %q, want manage-swarm", act)
	}
	if res.Kind != app.KindSwarm || res.ID != "sw1" {
		t.Fatalf("resource = %+v, want swarm sw1", res)
	}
}

func TestUnknownActor(t *testing.T) {
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	resp := h.do(t, "GET", "/swarms/sw1/actors/ghost/api/echo", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ─── проксирование и гигиена заголовков ─────────────────────────────────────

func TestAllowedProxyPassesPathAndBody(t *testing.T) {
	up := newTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	resp := h.do(t, "POST", "/swarms/sw1/actors/a1/api/echo",
		http.Header{"X-Custom": {"value1"}}, strings.NewReader("hello body"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (upstream status)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream ok" {
		t.Fatalf("body = %q", body)
	}
	_, ubody, _ := up.snapshot()
	if string(ubody) != "hello body" {
		t.Fatalf("upstream body = %q", ubody)
	}
	events := h.audit.snapshot()
	if len(events) != 1 || !events[0].Allowed {
		t.Fatalf("audit = %+v, want one allowed event", events)
	}
	if events[0].Method != "POST" {
		t.Fatalf("audit method = %q", events[0].Method)
	}
}

func TestTargetHeadersStripped(t *testing.T) {
	up := newTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", targetHeader(), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	hdr, _, _ := up.snapshot()
	// Клиентские значения не проходят: цель выбирает только сервер.
	if got := hdr.Get("X-Actor-Target"); got != "" {
		t.Fatalf("upstream X-Actor-Target = %q, want stripped", got)
	}
	if got := hdr.Get("X-Actor"); got != "a1" {
		t.Fatalf("upstream X-Actor = %q, want server-side a1", got)
	}
	if got := hdr.Get("X-Atespace"); got != "at1" {
		t.Fatalf("upstream X-Atespace = %q, want server-side at1", got)
	}
}

func TestTargetHeadersSetServerSide(t *testing.T) {
	up := newTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	hdr, _, host := up.snapshot()
	if hdr.Get(HeaderActor) != "a1" {
		t.Fatalf("upstream %s = %q, want a1", HeaderActor, hdr.Get(HeaderActor))
	}
	if hdr.Get(HeaderAtespace) != "at1" {
		t.Fatalf("upstream %s = %q, want at1", HeaderAtespace, hdr.Get(HeaderAtespace))
	}
	// Цель выбирает сервер, но Host сохраняется для проверки Origin актором.
	browser, err := url.Parse(h.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if host != browser.Host {
		t.Fatalf("upstream Host = %q, want browser host %q", host, browser.Host)
	}
}

func TestSessionCookieNotForwarded(t *testing.T) {
	up := newTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	req, err := http.NewRequest("GET", h.server.URL+"/swarms/sw1/actors/a1/api/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "session", Value: "secret-session-token"})
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "csrf-token"})
	req.AddCookie(&http.Cookie{Name: "other", Value: "kept"})
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	hdr, _, _ := up.snapshot()
	cookie := hdr.Get("Cookie")
	if strings.Contains(cookie, "session=") || strings.Contains(cookie, "csrf=") {
		t.Fatalf("upstream cookie = %q, session/csrf must not leak", cookie)
	}
	if !strings.Contains(cookie, "other=kept") {
		t.Fatalf("upstream cookie = %q, other=kept must stay", cookie)
	}
}

func TestUpstreamFailure502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", deadURL)
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestLargeBodyRejected(t *testing.T) {
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", newTestUpstream(t).base(t))
	big := bytes.Repeat([]byte("x"), int(DefaultMaxBodyBytes)+1024)
	resp := h.do(t, "POST", "/swarms/sw1/actors/a1/api/echo", nil, bytes.NewReader(big))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestUnknownRoute404(t *testing.T) {
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{})
	for _, uri := range []string{
		"/api/whatever", "/swarms/sw1/actors", "/swarms/sw1/actors/", "/swarms/sw1",
	} {
		resp := h.do(t, "GET", uri, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: status = %d, want 404", uri, resp.StatusCode)
		}
	}
}

// ─── WebSocket ─────────────────────────────────────────────────────────────

// wsMagicGUID — ключ RFC 6455 для Sec-WebSocket-Accept.
const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAccept(key string) string {
	sum := sha256.Sum256([]byte(key + wsMagicGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsTestUpstream — минимальный WebSocket-эхо-сервер на stdlib: handshake
// без внешней библиотеки и эхо кадров.
type wsTestUpstream struct {
	server  *httptest.Server
	gotConn chan struct{}
}

func newWSTestUpstream(t *testing.T) *wsTestUpstream {
	t.Helper()
	u := &wsTestUpstream{gotConn: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		if key == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"
		if _, err := rw.WriteString(resp); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		select {
		case u.gotConn <- struct{}{}:
		default:
		}
		wsEcho(rw, conn)
	})
	u.server = httptest.NewServer(mux)
	t.Cleanup(u.server.Close)
	return u
}

func (u *wsTestUpstream) base(t *testing.T) *url.URL {
	t.Helper()
	parsed, err := url.Parse(u.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// upstreamSawConnection — видел ли upstream handshake (без блокировки).
func (u *wsTestUpstream) sawConnection() bool {
	select {
	case <-u.gotConn:
		return true
	default:
		return false
	}
}

// wsEcho — эхо кадров: читает клиентский кадр, возвращает те же данные
// серверным кадром; close-кадр закрывает соединение.
func wsEcho(rw *bufio.ReadWriter, conn net.Conn) {
	for {
		payload, opcode, err := wsReadFrame(rw.Reader)
		if err != nil {
			return
		}
		if opcode == 8 { // close
			_ = wsWriteFrame(rw.Writer, 8, nil)
			_ = rw.Flush()
			return
		}
		if err := wsWriteFrame(rw.Writer, opcode, payload); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
	}
}

// wsReadFrame — чтение одного кадра (маска клиента снимается).
func wsReadFrame(r *bufio.Reader) (payload []byte, opcode byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return
	}
	opcode = hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return
		}
		length = int64(ext[0])<<8 | int64(ext[1])
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return
		}
		for _, b := range ext {
			length = length<<8 | int64(b)
		}
	}
	if length < 0 || length > 1<<20 {
		err = errors.New("frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// wsWriteFrame — серверный кадр без маски.
func wsWriteFrame(w io.Writer, opcode byte, payload []byte) error {
	hdr := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 65536:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		var ext [8]byte
		n64 := int64(n)
		for i := 7; i >= 0; i-- {
			ext[i] = byte(n64)
			n64 >>= 8
		}
		hdr = append(hdr, ext[:]...)
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// wsClientFrame — клиентский маскированный кадр.
func wsClientFrame(opcode byte, payload []byte) []byte {
	frame := []byte{0x80 | opcode}
	maskKey := make([]byte, 4)
	_, _ = rand.Read(maskKey)
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 65536:
		frame = append(frame, 0x80, 126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 0x80, 127)
		n64 := int64(n)
		var ext [8]byte
		for i := 7; i >= 0; i-- {
			ext[i] = byte(n64)
			n64 >>= 8
		}
		frame = append(frame, ext[:]...)
	}
	frame = append(frame, maskKey...)
	for i, b := range payload {
		frame = append(frame, b^maskKey[i%4])
	}
	return frame
}

// wsDialRaw — клиентский WebSocket-handshake сырым соединением через
// прокси: полный контроль над чтением после 101. Возвращает соединение
// и reader для серверных кадров.
func wsDialRaw(t *testing.T, serverURL, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rawKey := make([]byte, 16)
	if _, err := rand.Read(rawKey); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(rawKey)
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		t.Fatalf("read upgrade response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	return conn, reader
}

func TestWebsocketEchoThroughProxy(t *testing.T) {
	up := newWSTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	conn, reader := wsDialRaw(t, h.server.URL, "/swarms/sw1/actors/a1/ws")
	defer conn.Close()
	if _, err := conn.Write(wsClientFrame(1, []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	got, opcode, err := wsReadFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if opcode != 1 || string(got) != "hello" {
		t.Fatalf("echo frame opcode=%d payload=%q", opcode, got)
	}
	// Соединение живо и после первого обмена.
	if _, err := conn.Write(wsClientFrame(1, []byte("second"))); err != nil {
		t.Fatal(err)
	}
	got, _, err = wsReadFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("echo = %q", got)
	}
	// Аудит зафиксировал разрешённое соединение.
	events := h.audit.snapshot()
	if len(events) != 1 || !events[0].Allowed {
		t.Fatalf("audit = %+v", events)
	}
}

func TestWebsocketDeniedBeforeDial(t *testing.T) {
	up := newWSTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.res.set("at1", "a1", up.base(t))
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/ws", http.Header{
		"Upgrade":               {"websocket"},
		"Connection":            {"Upgrade"},
		"Sec-WebSocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
		"Sec-WebSocket-Version": {"13"},
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if up.sawConnection() {
		t.Fatal("upstream must not see denied connection")
	}
}

func TestWebsocketNonActiveSwarmRejected(t *testing.T) {
	up := newWSTestUpstream(t)
	swarms := fakeSwarms{"sw1": activeSwarm("sw1", "at1")}
	sw := swarms["sw1"]
	sw.State = app.StateDeleting
	swarms["sw1"] = sw
	h := newHarness(t, fixedUser("u1", "alice"), swarms)
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/ws", http.Header{
		"Upgrade":               {"websocket"},
		"Connection":            {"Upgrade"},
		"Sec-WebSocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
		"Sec-WebSocket-Version": {"13"},
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if up.sawConnection() {
		t.Fatal("upstream must not see non-active swarm connection")
	}
}

// ─── переоценка прав на живом соединении ────────────────────────────────────

// waitClosed — читает соединение до ошибки/EOF; true, если закрылось
// до дедлайна.
func waitClosed(conn net.Conn, reader *bufio.Reader, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 64)
		if _, err := reader.Read(buf); err != nil {
			return true
		}
	}
	return false
}

func TestRevocationClosesConnection(t *testing.T) {
	up := newWSTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	conn, reader := wsDialRaw(t, h.server.URL, "/swarms/sw1/actors/a1/ws")
	defer conn.Close()

	// Отзыв доступа: следующая переоценка обязана закрыть соединение.
	h.authz.set("u1", false)
	h.proxy.RecheckNow()
	if !waitClosed(conn, reader, 2*time.Second) {
		t.Fatal("connection must close after revocation")
	}
	// Переоценка действительно звала авторизатор.
	if h.authz.callCount() < 2 {
		t.Fatalf("authz calls = %d, want >= 2", h.authz.callCount())
	}
}

func TestRevocationClosesAfterExchanges(t *testing.T) {
	up := newWSTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	conn, reader := wsDialRaw(t, h.server.URL, "/swarms/sw1/actors/a1/ws")
	defer conn.Close()
	for _, msg := range []string{"one", "two"} {
		if _, err := conn.Write(wsClientFrame(1, []byte(msg))); err != nil {
			t.Fatal(err)
		}
		got, _, err := wsReadFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != msg {
			t.Fatalf("echo = %q, want %q", got, msg)
		}
	}
	h.authz.set("u1", false)
	h.proxy.RecheckNow()
	if !waitClosed(conn, reader, 2*time.Second) {
		t.Fatal("hijacked connection must close after revocation")
	}
}

func TestBackgroundRecheckRevokes(t *testing.T) {
	up := newWSTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	// Фоновый тикер: короткий интервал, реальная горутина, без ручного
	// recheckAll.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.proxy.cfg.RecheckInterval = 50 * time.Millisecond
	h.proxy.Start(ctx)
	conn, reader := wsDialRaw(t, h.server.URL, "/swarms/sw1/actors/a1/ws")
	defer conn.Close()
	h.authz.set("u1", false)
	if !waitClosed(conn, reader, 3*time.Second) {
		t.Fatal("background recheck must close revoked connection")
	}
}

// ─── конфигурация и дефолты ─────────────────────────────────────────────────

func TestConfigDefaults(t *testing.T) {
	if DefaultRecheckInterval != 60*time.Second {
		t.Fatalf("DefaultRecheckInterval = %v", DefaultRecheckInterval)
	}
	if DefaultMaxBodyBytes != 4<<20 {
		t.Fatalf("DefaultMaxBodyBytes = %d", DefaultMaxBodyBytes)
	}
}
