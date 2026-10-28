package server

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxy"
)

type viewResolver struct{ target *url.URL }

func (v viewResolver) ActorUpstream(_, _ string) (*url.URL, error) { return v.target, nil }

func TestActorViewCannotAcquirePortalAuthority(t *testing.T) {
	ts := newTestServer(t)
	admin, _, _, swarm := actorsSetup(t, ts)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Clear-Site-Data", `"cookies", "storage"`)
		w.Header().Set("Alt-Svc", `h3="attacker.test:443"`)
		w.Header().Set("WWW-Authenticate", `Basic realm="portal"`)
		w.Header().Add("Set-Cookie", "session=attacker; Path=/")
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-same-origin")
		io.WriteString(w, `<script>fetch('/api/admin/users',{method:'POST',body:document.cookie})</script>`)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	ts.srv.proxy.Close()
	ts.srv.proxy = proxy.New(proxy.Config{Swarms: ts.srv.gateway, Authorize: ts.srv.app.Can, CurrentUser: ts.srv.actorViewUser, Upstream: viewResolver{target}, RequestHook: ts.srv.actorViewRequest, ResponseHook: ts.srv.actorViewResponse, Revalidate: ts.srv.validProxySession})
	resp, err := admin.Get(ts.http.URL + "/api/swarms/" + swarm + "/proxy/actors/evil/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Request.URL.Path, "/actor-view/") {
		t.Fatalf("actor document still served on portal route: %s", resp.Request.URL.Path)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts;") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("actor document lacks enforced opaque sandbox: %q", csp)
	}
	for _, header := range []string{"Set-Cookie", "Clear-Site-Data", "Alt-Svc", "WWW-Authenticate"} {
		if len(resp.Header.Values(header)) != 0 {
			t.Errorf("actor can change portal browser state via %s", header)
		}
	}
	prefix := ts.http.URL + resp.Request.URL.Path
	for _, directive := range []string{"connect-src " + prefix + " ws" + strings.TrimPrefix(prefix, "http"), "form-action 'none'", "base-uri 'none'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("missing CSP directive %q", directive)
		}
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Fatal("missing referrer/opener isolation")
	}
}

func viewTestProxy(t *testing.T, ts *testServer, handler http.Handler) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	ts.srv.proxy.Close()
	ts.srv.proxy = proxy.New(proxy.Config{Swarms: ts.srv.gateway, Authorize: ts.srv.app.Can, CurrentUser: ts.srv.actorViewUser, Upstream: viewResolver{target}, RequestHook: ts.srv.actorViewRequest, ResponseHook: ts.srv.actorViewResponse, Revalidate: ts.srv.validProxySession})
}

func issueView(t *testing.T, ts *testServer, client *http.Client, swarm string) string {
	t.Helper()
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Get(ts.http.URL + "/api/swarms/" + swarm + "/proxy/actors/terminal/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("issue view status=%d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "/actor-view/") {
		t.Fatalf("not a view redirect: %q", location)
	}
	token := strings.Split(location, "/")[2]
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatal("ticket is not random 256-bit opaque data")
	}
	return location
}

func TestActorViewScopeAndRevocation(t *testing.T) {
	for _, revoke := range []string{"logout", "disabled", "membership", "transfer", "expired"} {
		t.Run(revoke, func(t *testing.T) {
			ts := newTestServer(t)
			admin, alice, _, swarm := actorsSetup(t, ts)
			var calls atomic.Int32
			viewTestProxy(t, ts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Origin") != ts.http.URL || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
					t.Error("incorrect upstream origin or leaked credentials")
				}
				io.WriteString(w, "asset")
			}))
			issuer := alice
			if revoke == "transfer" {
				issuer = admin
			}
			path := issueView(t, ts, issuer, swarm)
			get := func(path, origin string) int {
				req, _ := http.NewRequest("GET", ts.http.URL+path, nil)
				req.Header.Set("Origin", origin)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return resp.StatusCode
			}
			if get(path+"token", "null") != 200 {
				t.Fatal("valid scoped asset denied")
			}
			before := calls.Load()
			for _, bad := range []string{strings.Replace(path, "/terminal/", "/other/", 1), strings.Replace(path, swarm, "another-swarm", 1), "/actor-view/invalid/" + swarm + "/terminal/", path + "../other", path + "%2e%2e/other", path + "%252e%252e/other", path + "//other", path + "%2f../other"} {
				if got := get(bad, "null"); got != 404 {
					t.Errorf("unsafe path returned %d", got)
				}
			}
			if get(path, "https://attacker.test") != 403 {
				t.Fatal("foreign origin accepted")
			}
			if calls.Load() != before {
				t.Fatal("invalid ticket reached upstream")
			}
			req, _ := http.NewRequest("POST", ts.http.URL+"/api/admin/users", strings.NewReader(`{}`))
			req.Header.Set("Origin", "null")
			req.Header.Set("X-CSRFToken", ts.csrf(admin))
			for _, c := range admin.Jar.Cookies(req.URL) {
				req.AddCookie(c)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 403 {
				t.Fatal("management API accepted opaque origin")
			}
			sw, _, _ := ts.srv.gateway.SwarmByID(swarm)
			switch revoke {
			case "logout":
				resp, data, _ := ts.do(alice, "POST", "/api/auth/logout", ts.csrf(alice), nil)
				expectStatus(t, "logout", resp, data, 204)
			case "disabled":
				resp, data, _ := ts.do(admin, "POST", "/api/admin/users/alice/disable", ts.csrf(admin), nil)
				expectStatus(t, "disable", resp, data, 204)
			case "membership":
				resp, data, _ := ts.do(admin, "DELETE", "/api/projects/"+sw.ProjectID+"/members/alice", ts.csrf(admin), nil)
				expectStatus(t, "membership", resp, data, 204)
			case "transfer":
				resp, data, _ := ts.do(admin, "POST", "/api/swarms/"+swarm+"/transfer", ts.csrf(admin), map[string]string{"username": "bob"})
				expectStatus(t, "transfer", resp, data, 204)
			case "expired":
				key := strings.Split(path, "/")[2]
				ts.srv.views.mu.Lock()
				v := ts.srv.views.tickets[key]
				v.expires = time.Now().Add(-time.Second)
				ts.srv.views.tickets[key] = v
				ts.srv.views.mu.Unlock()
			}
			if get(path, "null") != 404 {
				t.Fatal("revoked ticket accepted")
			}
			if calls.Load() != before {
				t.Fatal("revoked ticket reached upstream")
			}
		})
	}
}

func TestActorViewRealWebSocket(t *testing.T) {
	for _, revoke := range []string{"logout", "viewer role"} {
		t.Run(revoke, func(t *testing.T) {
			ts := newTestServer(t)
			_, alice, bob, swarm := actorsSetup(t, ts)
			issuer := alice
			if revoke == "viewer role" {
				response, data, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "editor"})
				expectStatus(t, "grant terminal editor", response, data, 204)
				issuer = bob
			}
			viewTestProxy(t, ts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/ws" || r.Header.Get("Origin") != ts.http.URL || r.Header.Get("X-Actor") != "terminal" || r.Header.Get("Ate-Target-Actor") != swarmAtespace(t, ts.srv, swarm)+"/terminal" {
					t.Error("ticket websocket lost target, path, or check-origin")
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\nSet-Cookie: session=evil; Path=/\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
				rw.Flush()
				frame := make([]byte, 10)
				if _, err := io.ReadFull(rw, frame); err != nil {
					return
				}
				if frame[0] != 0x82 || frame[1] != 0x84 {
					t.Error("invalid masked websocket frame")
					return
				}
				for i := 0; i < 4; i++ {
					frame[6+i] ^= frame[2+i]
				}
				rw.Write(append([]byte{0x82, 4}, frame[6:]...))
				rw.Flush()
				io.Copy(io.Discard, rw)
			}))
			path := issueView(t, ts, issuer, swarm)
			req, _ := http.NewRequest("GET", ts.http.URL+path+"ws", nil)
			req.Header.Set("Origin", "null")
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
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
			resp, err := http.ReadResponse(reader, req)
			if err != nil || resp.StatusCode != 101 {
				t.Fatalf("ticket WS handshake failed: %v %v", resp, err)
			}
			if len(resp.Header.Values("Set-Cookie")) != 0 {
				t.Fatal("WS set portal cookie")
			}
			if resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
				t.Fatal("invalid websocket accept")
			}
			conn.Write([]byte{0x82, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'i' ^ 2, 'n' ^ 3, 'g' ^ 4})
			echo := make([]byte, 6)
			if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "\x82\x04ping" {
				t.Fatalf("echo=%q err=%v", echo, err)
			}
			if revoke == "logout" {
				response, data, _ := ts.do(issuer, "POST", "/api/auth/logout", ts.csrf(issuer), nil)
				expectStatus(t, "logout", response, data, 204)
			} else {
				response, data, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "viewer"})
				expectStatus(t, "revoke terminal write permission", response, data, 204)
				response, data, _ = ts.do(bob, "GET", "/api/auth/session", "", nil)
				expectStatus(t, "viewer session remains valid", response, data, 200)
				if data["authenticated"] != true {
					t.Fatal("role test was confounded by session revocation")
				}
				noRedirect := *bob
				noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				response, data, _ = ts.do(&noRedirect, "GET", "/api/swarms/"+swarm+"/proxy/actors/terminal/", "", nil)
				expectStatus(t, "viewer cannot mint a writable terminal ticket", response, data, 404)
			}
			ts.srv.proxy.RecheckNow()
			if _, err := reader.ReadByte(); err == nil {
				t.Fatal("ticket WS survived access revocation")
			} else if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("ticket WS not revoked")
			}
		})
	}
}

func TestActorViewRejectsRedirectAndBoundsTickets(t *testing.T) {
	ts := newTestServer(t)
	_, alice, _, swarm := actorsSetup(t, ts)
	viewTestProxy(t, ts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=evil; Path=/")
		http.Redirect(w, r, "/api/auth/session", 302)
	}))
	path := issueView(t, ts, alice, swarm)
	resp, err := http.Get(ts.http.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 || resp.Header.Get("Location") != "" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("actor redirect escaped isolated prefix")
	}
	ts.srv.views.mu.Lock()
	for i := len(ts.srv.views.tickets); i < actorViewLimit; i++ {
		ts.srv.views.tickets[fmt.Sprint(i)] = actorView{expires: time.Now().Add(time.Hour)}
	}
	ts.srv.views.mu.Unlock()
	resp, err = alice.Get(ts.http.URL + "/api/swarms/" + swarm + "/proxy/actors/terminal/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal("ticket store exceeded fixed bound")
	}
}
