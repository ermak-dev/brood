package telegram

// FEATURE: Outcome classes of Bot API calls (#280).
// SPEC: A 4xx answer is a definite refusal (*Error, ErrRefused): 401 is
// ErrUnauthorized, 403 ErrForbidden, 409 ErrConflict, 429 carries
// retry_after, capped at a day. A 4xx whose body is not a Bot API answer,
// such as a proxy page, is a refusal of no known kind (NotAPI). A request
// that never got a connection (a context done before sending, a DNS, dial or
// TLS handshake failure, TLS below 1.2, a deadline before the connection
// opened) wraps ErrNotSent and its masked cause: nothing was performed. Any
// other failure once a connection was open, a kept-alive one included
// (network, a request cut in its body, 5xx, a 3xx even with ok:true, a lost,
// truncated, oversized or malformed reply, cancellation or timeout in flight)
// wraps ErrUncertain: the method may have been performed, and net/http does
// not repeat it. The client reads at
// most maxBody+1 bytes of a response body, and a 2xx answer over maxBody also
// wraps ErrTooLarge. A 4xx stays a definite refusal even when its body read
// fails. Text from Telegram in an error is one line of at most maxReason
// bytes: control characters become spaces, and a cut never splits a code
// point.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// replying answers every request with status and body.
func replying(status int, body string) func(http.ResponseWriter, request) {
	return func(w http.ResponseWriter, _ request) { reply(w, status, body) }
}

func TestDefiniteRefusals(t *testing.T) {
	cases := []struct {
		name, body  string
		status      int
		code        int
		description string
		retry       time.Duration
		kind        error
		notAPI      bool
	}{
		{"bad token", `{"ok":false,"error_code":401,"description":"Unauthorized"}`, 401, 401, "Unauthorized", 0, ErrUnauthorized, false},
		{"blocked", `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, 403, 403, "Forbidden: bot was blocked by the user", 0, ErrForbidden, false},
		{"second poller", `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"}`, 409, 409, "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running", 0, ErrConflict, false},
		{"bad request", `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, 400, 400, "Bad Request: chat not found", 0, nil, false},
		{"rate limit", `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`, 429, 429, "Too Many Requests: retry after 7", 7 * time.Second, nil, false},
		{"refusal in a 200", `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`, 200, 400, "Bad Request: message is not modified", 0, nil, false},
		// Не ответ Bot API: метод не выполнен, но код не говорит о токене или чате.
		{"proxy page", `<html><body>403 Forbidden</body></html>`, 403, 403, "", 0, nil, true},
		{"proxy authentication page", `<html><body>401 Authorization Required</body></html>`, 401, 401, "", 0, nil, true},
		{"proxy conflict page", `<html><body>409 Conflict</body></html>`, 409, 409, "", 0, nil, true},
		{"proxy JSON", `{"error":"rate limited","retry_after":7}`, 429, 429, "", 0, nil, true},
		{"error code other than the status", `{"ok":false,"error_code":409,"description":"Conflict"}`, 403, 403, "", 0, nil, true},
		{"oversized refusal", `{"ok":false,"error_code":403,"description":"` + strings.Repeat("x", maxBody) + `"}`, 403, 403, "", 0, nil, true},
	}
	kinds := []error{ErrUnauthorized, ErrForbidden, ErrConflict}
	for _, tc := range cases {
		c, _ := fakeBot(t, replying(tc.status, tc.body))
		_, err := c.SendMessage(context.Background(), 777, "hi", nil)
		var e *Error
		if !errors.As(err, &e) || !errors.Is(err, ErrRefused) || errors.Is(err, ErrUncertain) || errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want a definite refusal", tc.name, err)
			continue
		}
		if e.Method != "sendMessage" || e.Code != tc.code || e.Description != tc.description || e.RetryAfter != tc.retry || e.NotAPI != tc.notAPI {
			t.Errorf("%s: refusal %+v", tc.name, *e)
		}
		if tc.notAPI != strings.Contains(err.Error(), "not a Bot API answer") {
			t.Errorf("%s: kind lost in %q", tc.name, err)
		}
		for _, kind := range kinds {
			if errors.Is(err, kind) != (kind == tc.kind) {
				t.Errorf("%s: errors.Is(%v) = %v", tc.name, kind, !(kind == tc.kind))
			}
		}
		if tc.description != "" && !strings.Contains(err.Error(), tc.description) {
			t.Errorf("%s: reason lost in %q", tc.name, err)
		}
	}
}

// A 4xx is a definite refusal even when its body breaks off: the status says
// the method was not performed, and a cut body is no Bot API answer.
func TestRefusalWithABrokenBodyIsDefinite(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
		hijack(t, w, "HTTP/1.1 403 Forbidden\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"ok\":false")
	})
	_, err := c.SendMessage(context.Background(), 777, "hi", nil)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrRefused) || errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotSent) || errors.Is(err, ErrForbidden) || hits.Load() != 1 {
		t.Fatalf("403 with a broken body: %v after %d requests, want a refusal of no known kind", err, hits.Load())
	}
	if *e != (Error{Method: "sendMessage", Code: 403, NotAPI: true}) {
		t.Fatalf("refusal %+v", *e)
	}
}

func TestReasonIsOneBoundedLine(t *testing.T) {
	c := newClient(t, Config{})
	for in, want := range map[string]string{
		"\tBad Request:\nchat\x00not\x1b[0m found\r\n": "Bad Request: chat not [0m found",
		"a\u0085b\x7fc\u009bd":                         "a b c d",
		"a" + strings.Repeat("я", 300):                 "a" + strings.Repeat("я", 255),
		strings.Repeat("b", 600):                       strings.Repeat("b", 512),
	} {
		if got := c.public(in); got != want {
			t.Errorf("public(%q) = %q, want %q", in, got, want)
		}
	}
	srv, _ := fakeBot(t, replying(400, `{"ok":false,"error_code":400,"description":"Bad Request:\nchat\u001b[31m not found\r"}`))
	_, err := srv.SendMessage(context.Background(), 777, "hi", nil)
	var e *Error
	if !errors.As(err, &e) || e.Description != "Bad Request: chat [31m not found" {
		t.Fatalf("refusal %v", err)
	}
}

// hijack takes the connection of a request that the server has fully read,
// writes head to it and drops it. It runs on a server goroutine and so
// reports a failure with t.Error.
func hijack(t *testing.T, w http.ResponseWriter, head string) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_, _ = io.WriteString(conn, head)
	_ = conn.Close()
}

func TestUncertainOutcomes(t *testing.T) {
	var redirected atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer elsewhere.Close()
	target := elsewhere.URL + "/bot" + botToken + "/sendMessage"
	cases := map[string]func(w http.ResponseWriter, r request){
		"500":              replying(500, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`),
		"502":              replying(502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`),
		"503 page":         replying(503, `<html>unavailable</html>`),
		"redirect":         func(w http.ResponseWriter, r request) { w.Header().Set("Location", target); reply(w, 307, "") },
		"not JSON":         replying(200, `not json`),
		"no result":        replying(200, `{"ok":true}`),
		"empty message":    replying(200, `{"ok":true,"result":{}}`),
		"wrong result":     replying(200, `{"ok":true,"result":true}`),
		"error code 500":   replying(200, `{"ok":false,"error_code":500,"description":"Internal"}`),
		"no error code":    replying(200, `{"ok":false}`),
		"oversized answer": replying(200, `{"ok":true,"result":`+strings.Repeat(" ", maxBody)+`true}`),
		"reply lost":       func(w http.ResponseWriter, r request) { hijack(t, w, "") },
		"truncated reply": func(w http.ResponseWriter, r request) {
			hijack(t, w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"ok\":true,\"result\":{\"message_id\":")
		},
	}
	for name, answer := range cases {
		c, hits := fakeBot(t, answer)
		_, err := c.SendMessage(context.Background(), 777, "hi", nil)
		if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotSent) || errors.Is(err, ErrRefused) || errors.Is(err, ErrInvalid) || hits.Load() != 1 {
			t.Errorf("%s: %v after %d requests, want uncertain", name, err, hits.Load())
		}
		if errors.Is(err, ErrTooLarge) != (name == "oversized answer") {
			t.Errorf("%s: errors.Is(ErrTooLarge) = %v", name, errors.Is(err, ErrTooLarge))
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("the client followed a redirect")
	}
}

// A 3xx is no answer of the Bot API even with ok:true in its body: the
// client follows no redirect, and the request may have been performed.
func TestRedirectWithOKIsUncertain(t *testing.T) {
	var followed atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		result(w, message(1, "hi"))
	}))
	defer elsewhere.Close()
	body, err := json.Marshal(map[string]any{"ok": true, "result": message(9, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{300, 301, 302, 303, 307, 308} {
		c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
			w.Header().Set("Location", elsewhere.URL+"/bot"+botToken+"/sendMessage")
			reply(w, status, string(body))
		})
		m, err := c.SendMessage(context.Background(), 777, "hi", nil)
		if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrRefused) || errors.Is(err, ErrNotSent) || m.MessageID != 0 || hits.Load() != 1 {
			t.Errorf("%d with ok:true: %+v, %v after %d requests, want uncertain", status, m, err, hits.Load())
			continue
		}
		if !strings.Contains(err.Error(), "unexpected HTTP status "+strconv.Itoa(status)) {
			t.Errorf("%d with ok:true: status lost in %q", status, err)
		}
	}
	if followed.Load() != 0 {
		t.Fatalf("the client followed %d redirects", followed.Load())
	}
}

// retry_after is capped at a day, so that no number of seconds overflows
// time.Duration.
func TestRetryAfterIsCappedAtADay(t *testing.T) {
	for seconds, want := range map[string]time.Duration{
		"1":     time.Second,
		"86399": 86399 * time.Second,
		"86400": 24 * time.Hour,
		"86401": 24 * time.Hour,
		// Последняя секунда, которую time.Duration вмещает, и первая сверх.
		"9223372036":          24 * time.Hour,
		"9223372037":          24 * time.Hour,
		"9223372036854775807": 24 * time.Hour,
		"0":                   0,
		"-7":                  0,
	} {
		c, _ := fakeBot(t, replying(429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":`+seconds+`}}`))
		_, err := c.SendMessage(context.Background(), 777, "hi", nil)
		var e *Error
		if !errors.As(err, &e) || e.NotAPI || e.RetryAfter != want {
			t.Errorf("retry_after %s: %v, want a refusal with a wait of %s", seconds, err, want)
		}
	}
}

func TestStatusAppearsInUncertainReason(t *testing.T) {
	c, _ := fakeBot(t, replying(502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`))
	_, err := c.SendMessage(context.Background(), 777, "hi", nil)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("reason %v", err)
	}
}

// notSent demands a failure before any connection: ErrNotSent, neither a
// refusal, nor an uncertain outcome, nor a local refusal.
func notSent(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotSent) || errors.Is(err, ErrUncertain) || errors.Is(err, ErrRefused) || errors.Is(err, ErrInvalid) {
		t.Errorf("%s: %v, want not sent", name, err)
	}
}

// listen accepts TCP connections on loopback and hands each to handle; it
// closes the listener and every connection at the end of the test.
func listen(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var open []net.Conn
	var done sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, conn := range open {
			_ = conn.Close()
		}
		mu.Unlock()
		done.Wait()
	})
	done.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			open = append(open, conn)
			mu.Unlock()
			done.Go(func() { defer conn.Close(); handle(conn) })
		}
	})
	return ln.Addr().String()
}

// noDNS is a resolver without a DNS server: every lookup fails without the
// network.
var noDNS = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("no DNS server in the test")
}}

// The cause in a failure is masked: errors.Is matches it as the original,
// errors.As reaches none of its values, and its text names it.
func TestConnectionNeverOpenedIsNotSent(t *testing.T) {
	ctx := context.Background()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err := newClient(t, Config{BaseURL: closed.URL}).SendMessage(ctx, 777, "hi", nil)
	notSent(t, "refused connection", err)
	if !errors.Is(err, syscall.ECONNREFUSED) || !strings.Contains(err.Error(), ": sendMessage: dial tcp ") {
		t.Errorf("refused connection: cause lost in %v", err)
	}
	if op := (*net.OpError)(nil); errors.As(err, &op) {
		t.Errorf("refused connection: errors.As reaches the cause %#v", op)
	}

	c := newClient(t, Config{BaseURL: "https://api.telegram.invalid"})
	c.http.Transport.(*http.Transport).DialContext = (&net.Dialer{Resolver: noDNS}).DialContext
	_, err = c.SendMessage(ctx, 777, "hi", nil)
	notSent(t, "DNS failure", err)
	if !strings.Contains(err.Error(), ": sendMessage: dial tcp: lookup api.telegram.invalid") {
		t.Errorf("DNS failure: cause lost in %v", err)
	}

	plain, hits := serve(t, func(w http.ResponseWriter, r request) { result(w, message(1, "hi")) })
	_, err = newClient(t, Config{BaseURL: strings.Replace(plain, "http://", "https://", 1)}).SendMessage(ctx, 777, "hi", nil)
	notSent(t, "TLS to a plain HTTP server", err)
	// net/http заменяет tls.RecordHeaderError ответа "HTTP/" своей ошибкой.
	if !errors.Is(err, http.ErrSchemeMismatch) {
		t.Errorf("TLS to a plain HTTP server: cause lost in %v", err)
	}

	untrusted := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0)
	untrusted.StartTLS()
	defer untrusted.Close()
	_, err = newClient(t, Config{BaseURL: untrusted.URL}).SendMessage(ctx, 777, "hi", nil)
	notSent(t, "untrusted certificate", err)
	if !strings.Contains(err.Error(), ": sendMessage: tls: failed to verify certificate: x509: ") {
		t.Errorf("untrusted certificate: cause lost in %v", err)
	}

	if hits.Load() != 0 {
		t.Fatalf("a failed connection reached a handler %d times", hits.Load())
	}
}

// TLS below 1.2 fails in the handshake, before a connection: nothing was
// sent. The client trusts the server certificate, so only the version fails.
func TestTLSBelowVersion12IsNotSent(t *testing.T) {
	var hits atomic.Int32
	old := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		result(w, message(1, "hi"))
	}))
	old.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}
	old.Config.ErrorLog = log.New(io.Discard, "", 0)
	old.StartTLS()
	defer old.Close()
	c := newClient(t, Config{BaseURL: old.URL})
	roots := x509.NewCertPool()
	roots.AddCert(old.Certificate())
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
	_, err := c.SendMessage(context.Background(), 777, "hi", nil)
	notSent(t, "TLS 1.1", err)
	if !strings.Contains(err.Error(), "protocol version") || hits.Load() != 0 {
		t.Fatalf("TLS 1.1: %v after %d requests", err, hits.Load())
	}
}

func TestDeadlineBeforeTheConnectionIsNotSent(t *testing.T) {
	// Сервер принимает TCP и молчит: TLS handshake не кончается до срока.
	addr := listen(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	c := newClient(t, Config{BaseURL: "https://" + addr, Timeout: 200 * time.Millisecond})
	_, err := c.SendMessage(context.Background(), 777, "hi", nil)
	notSent(t, "deadline in the TLS handshake", err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline in the TLS handshake: cause lost in %v", err)
	}
}

func TestFailureOnAnOpenConnectionIsUncertain(t *testing.T) {
	// Соединение открыто: запрос мог уйти, даже если сервер его не прочёл.
	addr := listen(t, func(net.Conn) {})
	c := newClient(t, Config{BaseURL: "http://" + addr})
	_, err := c.SendMessage(context.Background(), 777, "hi", nil)
	if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotSent) || errors.Is(err, ErrRefused) {
		t.Fatalf("connection closed by the server: %v", err)
	}
}

// A kept-alive connection is open before the request: a request lost on it
// may have been performed though no connection opened for it, and net/http
// does not repeat the POST on its own.
func TestFailureOnAReusedConnectionIsUncertain(t *testing.T) {
	var mu sync.Mutex
	var conns []string
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
		mu.Lock()
		conns = append(conns, r.conn)
		first := len(conns) == 1
		mu.Unlock()
		if first {
			result(w, message(1, "hi"))
			return
		}
		hijack(t, w, "")
	})
	ctx := context.Background()
	if _, err := c.SendMessage(ctx, 777, "hi", nil); err != nil {
		t.Fatal(err)
	}
	_, err := c.SendMessage(ctx, 777, "hi", nil)
	if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotSent) || errors.Is(err, ErrRefused) || hits.Load() != 2 {
		t.Fatalf("reply lost on a reused connection: %v after %d requests", err, hits.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if conns[0] != conns[1] {
		t.Fatalf("the second request came on a new connection: %v", conns)
	}
}

// The boundary of a sent request is the connection, not the end of the
// request: a request cut while its body is still going out has reached the
// server, so it is uncertain, not unsent.
func TestRequestCutWhileItsBodyGoesOutIsUncertain(t *testing.T) {
	var heads atomic.Int32
	addr := listen(t, func(conn net.Conn) {
		// Читает только заголовок и закрывает соединение с недочитанным телом.
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
			heads.Add(1)
		}
	})
	c := newClient(t, Config{BaseURL: "http://" + addr})
	// Малый буфер отправки: мегабайт тела не уходит в ядро целиком до
	// разрыва, и запись запроса падает, а не кончается.
	tr := c.http.Transport.(*http.Transport)
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := conn.(*net.TCPConn).SetWriteBuffer(4 << 10); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
	var cut atomic.Bool
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) { cut.Store(i.Err != nil) },
	})
	_, err := c.SendDocument(ctx, 777, Document{Name: "request.txt", Content: make([]byte, MaxDocumentBytes)}, nil)
	if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotSent) || errors.Is(err, ErrRefused) || heads.Load() != 1 {
		t.Fatalf("request cut in its body: %v after %d request heads", err, heads.Load())
	}
	if !cut.Load() {
		t.Fatal("the request went out whole: the test does not cut it in its body")
	}
}

func TestDoneContextSubmitsNothing(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, message(1, "hi")) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.SendMessage(ctx, 777, "hi", nil)
	notSent(t, "canceled before sending", err)
	if !errors.Is(err, context.Canceled) || hits.Load() != 0 {
		t.Fatalf("canceled before sending: %v after %d requests", err, hits.Load())
	}
}

func TestCancelInFlightIsUncertain(t *testing.T) {
	started := make(chan struct{})
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		close(started)
		<-r.gone
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := c.SendMessage(ctx, 777, "hi", nil)
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotSent) {
		t.Fatalf("canceled in flight: %v", err)
	}
}

func TestRequestTimeoutIsUncertain(t *testing.T) {
	base, _ := serve(t, func(w http.ResponseWriter, r request) { <-r.gone })
	c := newClient(t, Config{BaseURL: base, Timeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := c.GetMe(context.Background())
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotSent) || time.Since(start) > 5*time.Second {
		t.Fatalf("timed out request: %v after %s", err, time.Since(start))
	}
}

// cancelOnRead cancels the call on the first read of a body that ends
// cleanly but truncated.
type cancelOnRead struct {
	body   io.Reader
	cancel context.CancelFunc
}

func (b *cancelOnRead) Read(p []byte) (int, error) {
	b.cancel()
	return b.body.Read(p)
}
func (*cancelOnRead) Close() error { return nil }

type partialBody struct{ cancel context.CancelFunc }

func (t partialBody) RoundTrip(r *http.Request) (*http.Response, error) {
	body := &cancelOnRead{body: strings.NewReader(`{"ok":true,"result":true}`), cancel: t.cancel}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body, Request: r}, nil
}

func TestCanceledBodyReadIsNotParsed(t *testing.T) {
	c := newClient(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.http.Transport = partialBody{cancel: cancel}
	err := c.AnswerCallbackQuery(ctx, "q", "")
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.Canceled) {
		t.Fatalf("body read after cancellation: %v", err)
	}
}

// countingTransport counts in read the response body bytes the client takes
// from base.
type countingTransport struct {
	base http.RoundTripper
	read *atomic.Int64
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil {
		resp.Body = countingBody{resp.Body, t.read}
	}
	return resp, err
}

type countingBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// streaming answers a Bot API document padded with spaces to size bytes, in
// pieces, and stops when the client drops the reply.
func streaming(size int) func(http.ResponseWriter, request) {
	const doc = `{"ok":true,"result":true}`
	return func(w http.ResponseWriter, _ request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, doc); err != nil {
			return
		}
		pad := []byte(strings.Repeat(" ", 64<<10))
		for left := size - len(doc); left > 0; left -= len(pad) {
			if _, err := w.Write(pad[:min(left, len(pad))]); err != nil {
				return
			}
		}
	}
}

func TestResponseBodyIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		ok   bool
		read int64
	}{
		{"at the limit", maxBody, true, maxBody},
		{"one byte over", maxBody + 1, false, maxBody + 1},
		{"a stream of three limits", 3 * maxBody, false, maxBody + 1},
	} {
		c, _ := fakeBot(t, streaming(tc.size))
		var read atomic.Int64
		c.http.Transport = countingTransport{base: c.http.Transport, read: &read}
		err := c.AnswerCallbackQuery(context.Background(), "q", "")
		if tc.ok != (err == nil) || (!tc.ok && (!errors.Is(err, ErrUncertain) || !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrNotSent))) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if read.Load() != tc.read {
			t.Errorf("%s: the client read %d bytes of the body, want %d", tc.name, read.Load(), tc.read)
		}
	}
}
