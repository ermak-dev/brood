// Package telegram is a client of the Telegram Bot API on net/http and
// encoding/json: getMe, long polling getUpdates, sendMessage, sendDocument,
// editMessageReplyMarkup and answerCallbackQuery. It knows nothing of the
// store or the server, keeps no state between calls, never retries and
// writes no logs.
//
// A failed call is of one of four kinds, and only the last may have been
// performed:
//
//   - a local refusal (ErrInvalid): the call broke a Bot API limit and was
//     not sent;
//   - not sent (wraps ErrNotSent and its masked cause): the context was done
//     before sending, or no connection to the Bot API opened, because of DNS,
//     the dial, the TLS handshake or the deadline;
//   - a definite refusal (*Error, matching ErrRefused): Telegram answered
//     with a 4xx and did not perform the method. ErrUnauthorized is a bad
//     token (401), ErrForbidden a chat closed to the bot (403), ErrConflict a
//     second getUpdates poller or a webhook (409). A 4xx that is not a Bot
//     API answer, such as a proxy page, matches none of them (Error.NotAPI);
//   - an uncertain outcome (wraps ErrUncertain): a connection was open and no
//     definite answer came back, because of the network, a 5xx, a redirect,
//     a timeout or cancellation in flight, or a lost, oversized or malformed
//     reply; an oversized one also wraps ErrTooLarge. sendMessage is not
//     idempotent: the caller records such a message as unconfirmed and
//     repeats it only on an explicit request.
//
// A failure without an answer wraps a masked stand-in for its cause:
// errors.Is matches whatever the cause matched, such as
// context.DeadlineExceeded or syscall.ECONNREFUSED, but errors.As reaches
// none of the cause's values, since errors of net/http print the request URL
// and the token in it.
//
// The bot token travels only in the request path. NewClient takes it apart
// from Config, which holds no secret; errors mask it at every node of their
// Unwrap tree, Client prints a redacted form, and a Client value does not
// hold it.
package telegram

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultBaseURL is the public Bot API. An installation may point the
	// client at another origin, such as a local Bot API server.
	DefaultBaseURL = "https://api.telegram.org"
	// DefaultTimeout bounds one call; a long poll adds its wait to it.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout caps Config.Timeout.
	MaxTimeout = 10 * time.Minute
)

// maxBody bounds the memory one response takes, not the answers Telegram may
// send. JSON escapes take up to six bytes per UTF-16 unit, so a message at
// MessageLimit in reply to another such message takes about 53 KB with its
// user and chat fields, and a full batch of MaxUpdates of them about 5.3 MB.
// The bound leaves room over that for entities, keyboards and the other
// fields, but a batch heavy in them can still pass it. A larger answer wraps
// ErrTooLarge and is never parsed truncated; one update is far below the
// bound, so Poll.Limit 1 gets it through.
const maxBody = 8 << 20

// maxReason bounds the Telegram description or transport detail an error
// keeps, in bytes.
const maxReason = 512

// maxRetryAfter caps the retry_after a refusal carries, so that no number of
// seconds overflows time.Duration (past about 292 years).
const maxRetryAfter = 24 * time.Hour

// mask stands for a token in text.
const mask = "<redacted>"

var (
	// ErrInvalid is a local refusal: the call broke a Bot API limit and was
	// not sent.
	ErrInvalid = errors.New("invalid telegram request")
	// ErrRefused matches every definite refusal (*Error).
	ErrRefused = errors.New("telegram refused the request")
	// ErrUnauthorized matches a refusal of the bot token (401).
	ErrUnauthorized = errors.New("telegram rejected the bot token")
	// ErrForbidden matches a refusal of the chat (403), such as a user who
	// blocked the bot.
	ErrForbidden = errors.New("telegram forbade the request")
	// ErrConflict matches a conflict (409): another getUpdates poller of the
	// same bot or an active webhook.
	ErrConflict = errors.New("telegram reported a conflicting getUpdates poller or webhook")
	// ErrUncertain is wrapped by every failure that may have been performed.
	ErrUncertain = errors.New("telegram outcome uncertain")
	// ErrTooLarge is wrapped, with ErrUncertain, by a 2xx answer longer than
	// the client reads (maxBody). Such an answer is never parsed; a getUpdates
	// poller asks again with Poll.Limit 1 (GetUpdates).
	ErrTooLarge = errors.New("telegram response exceeds the size limit")
	// ErrNotSent is wrapped, with its masked cause, by a failure before the
	// request had a connection: a context done before sending, or a DNS, dial
	// or TLS handshake failure or the deadline on the way to a connection.
	// Nothing was performed, and a repeat is safe.
	ErrNotSent = errors.New("telegram request not sent")
)

// Error is a definite refusal: Telegram answered with a 4xx and did not
// perform the method. Code is the HTTP status or, in a 200 answer, the
// Bot API error_code; Description is Telegram's text with the token masked,
// empty when the answer had none.
type Error struct {
	Method      string
	Code        int
	Description string
	// RetryAfter is the wait Telegram asked for before a repeat (429),
	// capped at a day.
	RetryAfter time.Duration
	// NotAPI marks a 4xx whose body is not a Bot API answer, such as a page
	// of a proxy in front of the Bot API: the method was not performed, but
	// Code tells nothing of the token or the chat, so the refusal matches
	// none of ErrUnauthorized, ErrForbidden and ErrConflict.
	NotAPI bool
}

func (e *Error) Error() string {
	s := "telegram " + e.Method + " refused: " + strconv.Itoa(e.Code)
	if e.NotAPI {
		s += ", not a Bot API answer"
	}
	if e.Description != "" {
		s += " " + e.Description
	}
	if e.RetryAfter > 0 {
		s += "; retry after " + e.RetryAfter.String()
	}
	return s
}

// Is matches ErrRefused, and, unless NotAPI, ErrUnauthorized, ErrForbidden or
// ErrConflict by Code.
func (e *Error) Is(target error) bool {
	switch {
	case target == ErrRefused:
		return true
	case e.NotAPI:
		return false
	case target == ErrUnauthorized:
		return e.Code == http.StatusUnauthorized
	case target == ErrForbidden:
		return e.Code == http.StatusForbidden
	case target == ErrConflict:
		return e.Code == http.StatusConflict
	}
	return false
}

// Config holds the settings of a client. The bot token is not one of them:
// NewClient takes it apart, so a Config holds no secret and prints and
// marshals as any struct.
type Config struct {
	// BaseURL is the Bot API origin, empty for DefaultBaseURL. It is HTTPS,
	// or HTTP on a loopback address for a local Bot API server.
	BaseURL string
	// Timeout bounds one call, 0..MaxTimeout, zero for DefaultTimeout.
	Timeout time.Duration
}

// Client calls the Bot API for one bot. It is safe for concurrent use. It
// never follows redirects, which would carry the token elsewhere, and never
// takes a proxy from the environment.
type Client struct {
	base string
	// Токен -- за указателем на строку: такой указатель fmt печатает адресом
	// при любом глаголе, и там, где методов не зовёт (%p значения,
	// неэкспортированное поле), поэтому ни одна копия Client токена не несёт.
	token   *secret
	timeout time.Duration
	http    *http.Client
}

func (Client) String() string     { return "telegram.Client{redacted}" }
func (c Client) GoString() string { return c.String() }

// Format serves every verb of Client and *Client: String and GoString cover
// only some. Where fmt calls no methods it prints the fields, and they hold
// the token only by pointer.
func (c Client) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, c.String()) }

// secret is a bot token. It prints masked.
type secret string

func (secret) String() string               { return mask }
func (s secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

var (
	tokenPattern = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{1,128}$`)
	// botPath и bareToken -- токены любых ботов: в URL пути и голые.
	botPath   = regexp.MustCompile(`bot[0-9]+:[A-Za-z0-9_-]+`)
	bareToken = regexp.MustCompile(`[0-9]+:[A-Za-z0-9_-]{30,}`)
)

// NewClient checks the bot token, as BotFather issues it (<bot id>:<secret>),
// and the settings without contacting Telegram.
func NewClient(token string, cfg Config) (*Client, error) {
	base, err := baseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if !tokenPattern.MatchString(token) {
		return nil, errors.New("invalid telegram bot token: want <bot id>:<secret> as BotFather issues it")
	}
	timeout := cfg.Timeout
	if timeout < 0 || timeout > MaxTimeout {
		return nil, fmt.Errorf("telegram request timeout %s is outside 0..%s", timeout, MaxTimeout)
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	// Срок ответа задаёт дедлайн вызова, а не ResponseHeaderTimeout: long
	// poll законно молчит до своего ожидания.
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
		MaxIdleConns:           2,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        90 * time.Second,
		DisableCompression:     true,
	}
	held := secret(token)
	return &Client{base: base, token: &held, timeout: timeout, http: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// baseURL returns the origin of the setting: HTTPS, or HTTP on loopback,
// without credentials, path, query or fragment.
func baseURL(raw string) (string, error) {
	if raw == "" {
		return DefaultBaseURL, nil
	}
	fail := errors.New("telegram base URL must be an HTTPS origin, or an HTTP origin on a loopback address")
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", fail
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fail
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return "", fail
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", fail
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Close releases idle connections. Calls in flight keep their own deadlines.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// envelope is the Bot API answer: a result, or an error with its code.
type envelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int64 `json:"retry_after"`
	} `json:"parameters"`
}

// callJSON posts a method with JSON parameters.
func (c *Client) callJSON(ctx context.Context, method string, params any, wait time.Duration, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%w: cannot encode the %s parameters", ErrInvalid, method)
	}
	return c.call(ctx, method, "application/json", body, wait, out)
}

// call posts one Bot API method and decodes its result into out. wait, the
// long-poll wait, extends the call deadline.
func (c *Client) call(ctx context.Context, method, contentType string, body []byte, wait time.Duration, out any) error {
	if err := ctx.Err(); err != nil {
		return c.failure(ErrNotSent, method, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout+wait)
	defer cancel()
	// До соединения (DNS, dial, TLS handshake) запрос не ушёл: GotConn
	// net/http зовёт, когда соединение готово принять запрос.
	var connected atomic.Bool
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }})
	req, err := http.NewRequestWithContext(traced, http.MethodPost, c.base+"/bot"+string(*c.token)+"/"+method, bytes.NewReader(body))
	if err != nil {
		// Текст ошибки разбора несёт URL, а в нём токен.
		return fmt.Errorf("%w: cannot build the %s request", ErrInvalid, method)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return c.lost(ctx, method, err, connected.Load())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	// После отмены тело может кончиться чисто, но обрезанным: его не разбираем.
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return c.outcome(method, resp.StatusCode, raw, err, out)
}

// lost classifies a request that got no answer. Without a connection nothing
// was sent; once connected, the request may have been performed.
func (c *Client) lost(ctx context.Context, method string, err error, connected bool) error {
	cause := ctx.Err()
	if cause == nil {
		// *url.Error повторяет метод и URL запроса: в тексте -- только причина.
		cause = errors.New("request failed")
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			cause = ue.Err
		}
	}
	if !connected {
		return c.failure(ErrNotSent, method, cause)
	}
	return c.failure(ErrUncertain, method, cause)
}

// failed is a call without an answer: its kind, ErrNotSent or ErrUncertain,
// and its cause masked, both for errors.Is. The text has the token masked.
type failed struct {
	text  string
	kind  error
	cause error
}

func (e *failed) Error() string   { return e.text }
func (e *failed) Unwrap() []error { return []error{e.kind, e.cause} }

func (c *Client) failure(kind error, method string, cause error) error {
	m := c.maskCause(cause)
	return &failed{text: kind.Error() + ": " + method + ": " + m.text, kind: kind, cause: m}
}

// masked stands for the cause of a failure in its Unwrap tree: the text has
// the token masked, and errors.Is matches what the cause matched. Neither
// Unwrap, errors.As nor fmt reaches the cause itself: errors of net/http and
// of a transport print the request URL, and the token with it.
type masked struct {
	text string
	is   func(error) bool
}

func (e *masked) Error() string        { return e.text }
func (e *masked) Is(target error) bool { return e.is(target) }

func (c *Client) maskCause(cause error) *masked {
	// Причина -- только в замыкании: значение функции fmt печатает адресом,
	// а поле error напечатал бы при %#v со всеми полями.
	return &masked{text: c.public(cause.Error()), is: func(target error) bool { return errors.Is(cause, target) }}
}

// outcome classifies an answer of status with body raw, read with readErr.
func (c *Client) outcome(method string, status int, raw []byte, readErr error, out any) error {
	var env envelope
	parsed := readErr == nil && len(raw) <= maxBody && json.Unmarshal(raw, &env) == nil
	reason := ""
	if parsed && env.Description != "" {
		reason = ": " + env.Description
	}
	switch {
	case status >= 400 && status < 500:
		// Ответ 4xx -- явный отказ, даже если тело не прочлось. Ответ Bot API
		// -- ok:false с error_code, равным статусу; иное (страница прокси) --
		// отказ неизвестного рода.
		return c.refusal(method, status, env, parsed && !env.OK && env.ErrorCode == status)
	case status >= 500:
		return c.uncertain(method, "HTTP status "+strconv.Itoa(status)+reason)
	case status < 200 || status >= 300:
		return c.uncertain(method, "unexpected HTTP status "+strconv.Itoa(status))
	case readErr != nil:
		return c.failure(ErrUncertain, method, fmt.Errorf("response read failed: %w", readErr))
	case len(raw) > maxBody:
		return fmt.Errorf("%w: %s: %w of %d bytes", ErrUncertain, method, ErrTooLarge, maxBody)
	case !parsed:
		return c.uncertain(method, "response is not a Bot API answer")
	case !env.OK && env.ErrorCode >= 400 && env.ErrorCode < 500:
		return c.refusal(method, env.ErrorCode, env, true)
	case !env.OK:
		return c.uncertain(method, "error_code "+strconv.Itoa(env.ErrorCode)+reason)
	case json.Unmarshal(env.Result, out) != nil:
		return invalidResult(method)
	}
	return nil
}

// refusal builds the *Error of code; api tells whether env is a Bot API
// answer, the only one whose description and retry_after it keeps.
func (c *Client) refusal(method string, code int, env envelope, api bool) error {
	e := &Error{Method: method, Code: code, NotAPI: !api}
	if api {
		e.Description = c.public(env.Description)
		if s := env.Parameters.RetryAfter; s > 0 {
			e.RetryAfter = maxRetryAfter
			if s < int64(maxRetryAfter/time.Second) {
				e.RetryAfter = time.Duration(s) * time.Second
			}
		}
	}
	return e
}

func (c *Client) uncertain(method, detail string) error {
	return fmt.Errorf("%w: %s: %s", ErrUncertain, method, c.public(detail))
}

func invalidResult(method string) error {
	return fmt.Errorf("%w: %s: invalid result", ErrUncertain, method)
}

// public makes text from Telegram or the transport fit for an error: the
// token masked, control characters replaced, at most maxReason bytes.
func (c *Client) public(s string) string {
	s = redact(s, string(*c.token))
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if len(s) > maxReason {
		n := maxReason
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s
}

// redact masks token, its secret part and anything shaped like a bot token
// in s.
func redact(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, mask)
		if _, secret, ok := strings.Cut(token, ":"); ok && secret != "" {
			s = strings.ReplaceAll(s, secret, mask)
		}
	}
	s = botPath.ReplaceAllString(s, "bot"+mask)
	return bareToken.ReplaceAllString(s, mask)
}
