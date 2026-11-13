package telegram

// FEATURE: The bot token stays out of errors, formatting and logs (#280).
// SPEC: The token travels only in the request path. Errors of every outcome
// class mask it at every node of their Unwrap tree, including transport
// errors that net/http prints with the full URL and Telegram descriptions
// that echo it: the cause of a failure is masked, errors.Is matches it as the
// original and errors.As finds no *url.Error. NewClient takes the token apart
// from Config, so no Config shows it however it is printed or marshaled.
// Client prints a redacted form for every fmt verb. A Client keeps the token
// outside its value, so no copy of a Client shows it, even where fmt reads
// the fields without calling methods. The package writes no logs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"
)

// otherToken is a token of some other bot in the shape Telegram issues.
const otherToken = "987654321:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

// showsToken tells whether s holds the test token, its secret, or the token
// or secret of the other bot.
func showsToken(s string) bool {
	for _, secret := range []string{botToken, tokenSecret, otherToken, otherToken[strings.Index(otherToken, ":")+1:]} {
		if strings.Contains(s, secret) {
			return true
		}
	}
	return false
}

// printVerbs are the verbs a logger or an error wrapper prints a value with.
var printVerbs = []string{"%v", "%+v", "%#v", "%s", "%q"}

// noToken formats every value with printVerbs and demands that no token
// shows.
func noToken(t *testing.T, name string, values ...any) {
	t.Helper()
	for _, v := range values {
		for _, verb := range printVerbs {
			if got := fmt.Sprintf(verb, v); showsToken(got) {
				t.Errorf("%s: %s shows a token: %s", name, verb, got)
			}
		}
	}
}

// tokenNodes walks the whole Unwrap tree of err, through Unwrap() error and
// Unwrap() []error, and names every node whose Error or printVerbs form
// shows a token.
func tokenNodes(err error) []string {
	var found []string
	var walk func(e error, path string)
	walk = func(e error, path string) {
		if e == nil {
			return
		}
		forms := map[string]string{"Error()": e.Error()}
		for _, verb := range printVerbs {
			forms[verb] = fmt.Sprintf(verb, e)
		}
		for form, s := range forms {
			if showsToken(s) {
				found = append(found, fmt.Sprintf("%s (%T) %s: %s", path, e, form, s))
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap(), path+".0")
		case interface{ Unwrap() []error }:
			for i, next := range u.Unwrap() {
				walk(next, fmt.Sprintf("%s.%d", path, i))
			}
		}
	}
	walk(err, "err")
	return found
}

// chainHasNoToken demands that no node of the Unwrap tree of err shows a
// token and that errors.As finds no *url.Error, which prints the request URL.
func chainHasNoToken(t *testing.T, name string, err error) {
	t.Helper()
	for _, node := range tokenNodes(err) {
		t.Errorf("%s: node %s", name, node)
	}
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		t.Errorf("%s: errors.As reaches the *url.Error of %q", name, ue.URL)
	}
}

// hiddenToken is an error whose text is clean and whose fields hold a token,
// as %#v shows them.
type hiddenToken struct{ token string }

func (hiddenToken) Error() string { return "clean" }

// wrapped is an error with clean text over a single cause.
type wrapped struct{ cause error }

func (wrapped) Error() string   { return "clean" }
func (e wrapped) Unwrap() error { return e.cause }

func TestTokenNodesWalksTheWholeTree(t *testing.T) {
	leak := &url.Error{Op: "Post", URL: "https://api.telegram.org/bot" + botToken + "/getMe", Err: io.EOF}
	err := wrapped{&failed{text: "clean", kind: ErrUncertain, cause: errors.Join(errors.New("clean"), wrapped{leak}, hiddenToken{botToken})}}
	if err.Error() != "clean" {
		t.Fatalf("the root shows the leak itself: %q", err)
	}
	got := strings.Join(tokenNodes(err), "\n")
	for _, node := range []string{"err.0.1.1.0 (*url.Error) Error()", "err.0.1.2 (telegram.hiddenToken) %#v"} {
		if !strings.Contains(got, node+":") {
			t.Errorf("walk missed %s in:\n%s", node, got)
		}
	}
	if strings.Contains(got, "err (") || strings.Contains(got, "err.0 (") || strings.Contains(got, "err.0.1.0 (") {
		t.Errorf("walk blamed a clean node:\n%s", got)
	}
}

// urlTransport fails the way net/http does deeper in a call, with an error
// that carries the request URL, and the token in it. With body set it
// answers 200 and fails the read of the body that way instead.
type urlTransport struct{ body bool }

func (t urlTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.body {
		return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: syscall.ECONNRESET}
	}
	body := io.NopCloser(iotest.ErrReader(&url.Error{Op: "read", URL: r.URL.String(), Err: context.Canceled}))
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body, Request: r}, nil
}

func TestErrorsNeverShowTheToken(t *testing.T) {
	ctx := context.Background()
	errs := map[string]error{}
	// net/http печатает в *url.Error полный URL запроса, а с ним и токен.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, errs["refused dial"] = newClient(t, Config{BaseURL: closed.URL}).SendMessage(ctx, 777, "hi", nil)
	dns := newClient(t, Config{BaseURL: "https://api.telegram.invalid"})
	dns.http.Transport.(*http.Transport).DialContext = (&net.Dialer{Resolver: noDNS}).DialContext
	_, errs["DNS failure"] = dns.GetMe(ctx)
	plain, _ := serve(t, func(w http.ResponseWriter, r request) { result(w, true) })
	_, errs["HTTPS to an HTTP server"] = newClient(t, Config{BaseURL: strings.Replace(plain, "http://", "https://", 1)}).GetMe(ctx)
	untrusted := httptest.NewUnstartedServer(http.NotFoundHandler())
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0)
	untrusted.StartTLS()
	defer untrusted.Close()
	_, errs["untrusted certificate"] = newClient(t, Config{BaseURL: untrusted.URL}).GetMe(ctx)
	slow, _ := serve(t, func(w http.ResponseWriter, r request) { <-r.gone })
	_, errs["timeout"] = newClient(t, Config{BaseURL: slow, Timeout: 100 * time.Millisecond}).GetMe(ctx)
	lost, _ := fakeBot(t, func(w http.ResponseWriter, r request) { hijack(t, w, "") })
	_, errs["reply lost on an open connection"] = lost.SendMessage(ctx, 777, "hi", nil)
	carrying := newClient(t, Config{})
	carrying.http.Transport = urlTransport{}
	_, errs["transport failure carrying the URL"] = carrying.SendMessage(ctx, 777, "hi", nil)
	reading := newClient(t, Config{})
	reading.http.Transport = urlTransport{body: true}
	_, errs["body read failure carrying the URL"] = reading.SendMessage(ctx, 777, "hi", nil)
	echo := func(status int) *Client {
		c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
			reply(w, status, fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"Not Found: /bot%s/%s, secret %s, other /bot%s/getMe"}`, status, botToken, r.method, tokenSecret, otherToken))
		})
		return c
	}
	_, errs["echoing refusal"] = echo(404).SendMessage(ctx, 777, "hi", nil)
	_, errs["echoing server error"] = echo(500).SendMessage(ctx, 777, "hi", nil)
	_, errs["echoing refusal in a 200"] = echo(200).SendMessage(ctx, 777, "hi", nil)
	echoBody := func(status int, body string) *Client {
		c, _ := fakeBot(t, replying(status, body+" /bot"+botToken+"/sendMessage "+tokenSecret))
		return c
	}
	_, errs["echoing proxy page"] = echoBody(403, "<html>Forbidden").SendMessage(ctx, 777, "hi", nil)
	_, errs["echoing gateway page"] = echoBody(502, "<html>Bad Gateway").SendMessage(ctx, 777, "hi", nil)
	_, errs["echoing malformed body"] = echoBody(200, `{"ok":true,"result":`).SendMessage(ctx, 777, "hi", nil)
	_, errs["bad token"] = NewClient(tokenSecret+"/x", Config{})
	for name, err := range errs {
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		noToken(t, name, err)
		chainHasNoToken(t, name, err)
	}
	if e, ok := errs["echoing refusal"].(*Error); !ok || !strings.Contains(e.Description, "/sendMessage") {
		t.Errorf("masking dropped the description: %v", errs["echoing refusal"])
	}
	// Маска причины сохраняет её классы для errors.Is.
	if err := errs["transport failure carrying the URL"]; !errors.Is(err, ErrNotSent) || !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("masking lost the transport cause: %v", err)
	}
	if err := errs["body read failure carrying the URL"]; !errors.Is(err, ErrUncertain) || !errors.Is(err, context.Canceled) {
		t.Errorf("masking lost the read cause: %v", err)
	}
}

func TestRedactMasksTokens(t *testing.T) {
	for _, s := range []string{
		`Post "https://api.telegram.org/bot` + botToken + `/sendMessage": dial tcp: connection refused`,
		"the secret alone: " + tokenSecret,
		"another bot: https://api.telegram.org/bot" + otherToken + "/getMe",
	} {
		noToken(t, s, redact(s, botToken))
	}
	got := redact(`Post "https://api.telegram.org/bot`+botToken+`/sendMessage": EOF`, botToken)
	if !strings.Contains(got, "https://api.telegram.org/bot") || !strings.Contains(got, `/sendMessage": EOF`) {
		t.Fatalf("redact lost the context: %q", got)
	}
}

// Each pattern masks what the other misses: a bare token of another bot
// outside a bot path, and a bot path whose secret is too short to pass for a
// bare token. Clock times and other number pairs stay.
func TestRedactPatternsStandAlone(t *testing.T) {
	const shortPath = "/bot55:short-secret_x/getMe"
	for in, want := range map[string]string{
		"Bad Request: token " + otherToken + " is not valid": "Bad Request: token " + mask + " is not valid",
		"Not Found: " + shortPath:                            "Not Found: /bot" + mask + "/getMe",
		"retry at 12:30:45 for chat 777:42":                  "retry at 12:30:45 for chat 777:42",
	} {
		if got := redact(in, botToken); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	c, _ := fakeBot(t, replying(400, `{"ok":false,"error_code":400,"description":"Bad Request: token `+otherToken+` and `+shortPath+`"}`))
	_, err := c.SendMessage(context.Background(), 777, "hi", nil)
	var e *Error
	if !errors.As(err, &e) || e.Description != "Bad Request: token "+mask+" and /bot"+mask+"/getMe" {
		t.Fatalf("description of a refusal: %v", err)
	}
}

// redactVerbs covers every fmt verb with the flags a caller can write; %T
// and %p never reach Format.
func redactVerbs() []string {
	var out []string
	for _, v := range "vsqxXdtcbeEfFgGoOU" {
		for _, flags := range []string{"", "+", "#", "-", " ", "0", "10", "-10", "+#"} {
			out = append(out, "%"+flags+string(v))
		}
	}
	return out
}

func TestSecretTypesRedactEveryFormatVerb(t *testing.T) {
	c := newClient(t, Config{BaseURL: "https://api.telegram.org", Timeout: time.Second})
	token := secret(botToken)
	type nested struct {
		P *Client
		C Client
		A any
	}
	for name, tc := range map[string]struct {
		want   string
		values []any
	}{
		"Client": {"telegram.Client{redacted}", []any{c, *c, []*Client{c}, []Client{*c}, map[string]*Client{"k": c}}},
		"secret": {mask, []any{token, &token, []secret{token}}},
		"nested": {"redacted", []any{nested{c, *c, c}}},
	} {
		for _, v := range tc.values {
			for _, verb := range redactVerbs() {
				got := fmt.Sprintf(verb, v)
				if strings.Contains(got, tokenSecret) || !strings.Contains(got, tc.want) {
					t.Fatalf("%s %s = %q, want the redacted form %q", name, verb, got, tc.want)
				}
			}
		}
	}
}

func TestClientValueNeverShowsTheToken(t *testing.T) {
	// Config, с которым клиент создан, вызывающий держит рядом: токена в нём
	// нет, NewClient берёт его отдельно.
	cfg := Config{BaseURL: "https://api.telegram.org", Timeout: time.Second}
	c := newClient(t, cfg)
	for _, v := range []any{c, *c} {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			if got := fmt.Sprintf(verb, v); got != "telegram.Client{redacted}" {
				t.Errorf("%T %s = %q", v, verb, got)
			}
		}
	}
	// fmt не зовёт методов значения в неэкспортированном поле и при %p
	// значения: печатает поля, и токена в них быть не должно.
	type held struct{ c Client }
	type heldPointer struct{ c *Client }
	type heldConfig struct{ cfg Config }
	type heldConfigPointer struct{ cfg *Config }
	values := []any{c, *c, []Client{*c}, held{*c}, &held{*c}, heldPointer{c}, struct{ C Client }{*c},
		cfg, &cfg, heldConfig{cfg}, &heldConfig{cfg}, heldConfigPointer{&cfg}, struct{ C Config }{cfg}}
	for _, v := range values {
		for _, verb := range append(redactVerbs(), "%p") {
			if got := fmt.Sprintf(verb, v); strings.Contains(got, tokenSecret) {
				t.Errorf("%T %s shows the token: %s", v, verb, got)
			}
		}
		b, err := json.Marshal(v)
		if err != nil || strings.Contains(string(b), tokenSecret) {
			t.Errorf("JSON of %T: %s, %v", v, b, err)
		}
	}
}
