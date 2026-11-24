// Package proxy forwards HTTP and WebSocket requests to the terminal of an
// agent. The server picks the agent from the swarm and the agent name and
// hands the proxy a Target; the client controls neither the address nor any
// header that could select another agent. Access is checked before the
// connection and re-evaluated on an interval: revoked access closes live
// connections. The library stays silent: decisions go to the audit callback,
// refusals are statuses with a plain reason.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"brood.dev/go/brood/app"
)

// Defaults; the configuration overrides them.
const (
	// DefaultRecheckInterval is the period of access re-evaluation for live connections.
	DefaultRecheckInterval = 60 * time.Second
	// DefaultMaxBodyBytes caps the body of a non-WebSocket request.
	DefaultMaxBodyBytes int64 = 4 << 20
	// DefaultDialTimeout caps opening a stream to the agent terminal.
	DefaultDialTimeout = 10 * time.Second
	// DefaultResponseHeaderTimeout caps the wait for the status line of the
	// agent terminal on a non-WebSocket request.
	DefaultResponseHeaderTimeout = 60 * time.Second
)

// Имена cookie сессии и CSRF brood: к агенту не идут никогда.
const (
	cookieSession = "session"
	cookieCSRF    = "csrf"
)

// credentialHeaders — учётные данные панели: к агенту не идут.
var credentialHeaders = []string{"Authorization", "X-CSRFToken"}

// hopHeaders — hop-by-hop заголовки RFC 7230; переносятся вручную
// только Upgrade и Connection в upgrade-запросе.
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

const (
	headerUpgrade    = "Upgrade"
	headerConnection = "Connection"
)

// ErrUnknownAgent reports that the swarm has no reachable agent of that name.
var ErrUnknownAgent = errors.New("agent is not reachable in its swarm")

// Target is the agent the server selected for a request: the container UID
// and its generation. It comes from the server's records, never from the
// request.
type Target struct {
	UID        string
	Generation int64
}

// Identity is an authenticated user: the ID for app.Can and the name for audit.
type Identity struct {
	ID       string
	Username string
}

// CurrentUser returns the user of an authenticated request; ok=false means
// an anonymous request. The proxy does no authentication itself.
type CurrentUser func(ctx context.Context) (Identity, bool)

// Authorizer checks access; the implementation is app.Service.Can with
// app.ActionManageSwarm on app.SwarmRes. An error is an internal failure,
// not a refusal.
type Authorizer func(userID string, action app.Action, res app.Resource) (bool, error)

// SwarmLookup reads a swarm by ID; the implementation is app.Store.
type SwarmLookup interface {
	SwarmByID(id string) (app.Swarm, bool, error)
}

// AuditEvent is one decision of the proxy: who, where, how and whether it
// was allowed. The fields are flat for slog.Attr.
type AuditEvent struct {
	Username string
	SwarmID  string
	Agent    string
	Method   string
	Allowed  bool
	// Reason is the context of the decision (not permitted, swarm is not
	// active, access revoked and so on); empty for allowed requests.
	Reason string
}

// Audit receives audit events; nil disables audit.
type Audit func(event AuditEvent)

// UpgradeOutcome describes submission, not completion of upstream effects.
type UpgradeOutcome uint8

const (
	// UpgradeNotSubmitted proves that no upstream request write was attempted.
	UpgradeNotSubmitted UpgradeOutcome = iota
	// UpgradeSubmissionUnknown means a write may have reached upstream. Neither
	// an HTTP refusal nor a failed client handshake proves absence of effects.
	UpgradeSubmissionUnknown
)

// UpgradeReservation fences a writable upgrade at its final submission boundary.
type UpgradeReservation struct {
	// BeforeSubmit runs after dialing, immediately before the first request write.
	// Its release function is called after the write/flush attempt, before waiting
	// for any response or streaming bytes. An error prevents submission.
	BeforeSubmit func() (release func(), err error)
	// Finish runs after both connections close, including pre-submit failures.
	Finish func(UpgradeOutcome)
}

// Config holds the dependencies and settings of the proxy.
type Config struct {
	// Resolve selects the agent of the swarm by name. ErrUnknownAgent answers
	// 404; an *app.APIError with status 503 passes its detail to the client.
	Resolve func(swarmID, agent string) (Target, error)
	// Dial opens a stream to the terminal of the target. The context bounds
	// the opening and keeps the stream alive until Close.
	Dial func(ctx context.Context, target Target) (net.Conn, error)
	// TargetValid confirms on long-lived requests that the target generation
	// is still the reachable one; nil skips the check.
	TargetValid func(Target) bool
	// Host is the address of the terminal inside the agent. It is the Host and
	// the Origin of every request to the agent: the client's own Origin is
	// checked before the proxy.
	Host string
	// RequestHook applies server-controlled policy after outbound sanitization.
	RequestHook func(out, original *http.Request)
	// ResponseHook applies server-controlled policy before any headers are sent.
	ResponseHook func(original *http.Request, response *http.Response) error
	// BeginUpgrade reserves mutation authority on the target before upgrade I/O.
	BeginUpgrade func(*http.Request, Target) (UpgradeReservation, error)
	// Revalidate checks the original request, including while an upgrade is pending.
	Revalidate func(*http.Request) bool
	// RevalidateStream replaces Revalidate after a successful upgrade.
	RevalidateStream func(*http.Request) bool
	// Swarms reads swarms by ID.
	Swarms SwarmLookup
	// Authorize is the access gate before any connection.
	Authorize Authorizer
	// CurrentUser returns the user from the request context.
	CurrentUser CurrentUser
	// Audit receives decisions; nil is allowed.
	Audit Audit
	// RecheckInterval is the period of access re-evaluation for live
	// connections; 0 means DefaultRecheckInterval.
	RecheckInterval time.Duration
	// MaxBodyBytes caps a non-WebSocket body; 0 means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// DialTimeout caps opening a stream; 0 means DefaultDialTimeout.
	DialTimeout time.Duration
	// ResponseHeaderTimeout caps the wait for the status line; 0 means
	// DefaultResponseHeaderTimeout.
	ResponseHeaderTimeout time.Duration
	// Logger is the slog logger; nil means slog.Default().
	Logger *slog.Logger
}

// Proxy forwards requests to agent terminals and tracks live connections.
type Proxy struct {
	cfg    Config
	logger *slog.Logger

	mu    sync.Mutex
	conns map[*liveConn]struct{}
}

// liveConn — живое соединение с отслеживаемым доступом.
// close зовут и переоценка, и завершение копирования: флаг страхует
// от двойного закрытия, параллельные Close TCPConn безопасны.
type liveConn struct {
	user   Identity
	swarm  app.Swarm
	agent  string
	mu     sync.Mutex
	closed bool
	client net.Conn
	server net.Conn
	cancel context.CancelFunc
	valid  func() bool
}

func (lc *liveConn) close() {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.closed {
		return
	}
	lc.closed = true
	if lc.cancel != nil {
		lc.cancel()
	}
	if lc.client != nil {
		_ = lc.client.Close()
	}
	if lc.server != nil {
		_ = lc.server.Close()
	}
}

// New builds the proxy. Resolve, Dial, Host, Swarms, Authorize and
// CurrentUser are required; zero intervals take the defaults.
func New(cfg Config) *Proxy {
	if cfg.RecheckInterval <= 0 {
		cfg.RecheckInterval = DefaultRecheckInterval
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.ResponseHeaderTimeout <= 0 {
		cfg.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{cfg: cfg, logger: logger, conns: make(map[*liveConn]struct{})}
}

// transport — RoundTripper одного запроса к одной цели. Общий пул
// соединений не нужен: поток ведёт к конкретному агенту и другому
// запросу достаться не должен.
func (p *Proxy) transport(target Target) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.dial(ctx, target)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: p.cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

type agentConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *agentConn) Close() error { c.cancel(); return c.Conn.Close() }

// dial открывает поток к терминалу цели. Поток relay живёт, пока жив
// контекст Dial: таймер ограничивает только открытие, контекст
// отпускает Close.
func (p *Proxy) dial(ctx context.Context, target Target) (net.Conn, error) {
	lifetime, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(p.cfg.DialTimeout, cancel)
	conn, err := p.cfg.Dial(lifetime, target)
	stopped := timer.Stop()
	if err != nil || !stopped || lifetime.Err() != nil {
		cancel()
		if conn != nil {
			conn.Close()
		}
		if err != nil {
			return nil, err
		}
		return nil, context.DeadlineExceeded
	}
	return &agentConn{Conn: conn, cancel: cancel}, nil
}

// Close terminates active requests and upgraded connections.
func (p *Proxy) Close() {
	p.mu.Lock()
	current := make([]*liveConn, 0, len(p.conns))
	for lc := range p.conns {
		current = append(current, lc)
	}
	p.mu.Unlock()
	for _, lc := range current {
		p.untrack(lc)
	}
}

// Start runs the access re-evaluation of live connections until ctx is
// canceled. Without it the proxy still works but revokes access only on an
// explicit RecheckNow.
func (p *Proxy) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(p.cfg.RecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				p.Close()
				return
			case <-ticker.C:
				p.RecheckNow()
			}
		}
	}()
}

// RecheckNow re-evaluates access of all live connections and closes the
// revoked ones. The server may call it on an event without waiting for the
// ticker.
func (p *Proxy) RecheckNow() {
	p.mu.Lock()
	current := make([]*liveConn, 0, len(p.conns))
	for lc := range p.conns {
		current = append(current, lc)
	}
	p.mu.Unlock()

	var checks sync.WaitGroup
	for _, lc := range current {
		checks.Add(1)
		go func() {
			defer checks.Done()
			if lc.valid != nil && !lc.valid() {
				p.revoke(lc, "session revoked")
				return
			}
			allowed, err := p.cfg.Authorize(lc.user.ID, app.ActionManageSwarm, app.SwarmRes(lc.swarm.ID))
			switch {
			case err != nil:
				// Сбой проверки небезопасен: закрываем соединение.
				p.revoke(lc, "authorization check failed")
			case !allowed:
				p.revoke(lc, "access revoked")
			default:
				if sw, ok, err := p.cfg.Swarms.SwarmByID(lc.swarm.ID); err != nil || !ok || sw.State != app.StateActive {
					p.revoke(lc, "swarm is not active")
				}
			}
		}()
	}
	checks.Wait()
}

// revoke закрывает соединение, снимает с реестра и пишет аудит.
func (p *Proxy) revoke(lc *liveConn, reason string) {
	p.mu.Lock()
	delete(p.conns, lc)
	p.mu.Unlock()
	lc.close()
	p.emit(AuditEvent{
		Username: lc.user.Username, SwarmID: lc.swarm.ID, Agent: lc.agent,
		Allowed: false, Reason: reason,
	})
}

func (p *Proxy) targetCheck(r *http.Request, target Target, revalidate func(*http.Request) bool) func() bool {
	if revalidate == nil && p.cfg.TargetValid == nil {
		return nil
	}
	return func() bool {
		return (revalidate == nil || revalidate(r)) && (p.cfg.TargetValid == nil || p.cfg.TargetValid(target))
	}
}

// track регистрирует живое соединение.
func (p *Proxy) track(lc *liveConn) {
	p.mu.Lock()
	p.conns[lc] = struct{}{}
	p.mu.Unlock()
}

// untrack снимает соединение по завершении и закрывает обе стороны.
func (p *Proxy) untrack(lc *liveConn) {
	p.mu.Lock()
	delete(p.conns, lc)
	p.mu.Unlock()
	lc.close()
}

// emit отправляет событие в аудит-колбэк и slog.
func (p *Proxy) emit(e AuditEvent) {
	if p.cfg.Audit != nil {
		p.cfg.Audit(e)
	}
	p.logger.Info("proxy access",
		"user", e.Username,
		"swarm", e.SwarmID,
		"agent", e.Agent,
		"method", e.Method,
		"allowed", e.Allowed,
		"reason", e.Reason)
}

// Serve forwards r to the terminal of the agent named agent in swarm
// swarmID. path is the path inside the terminal, "/" for its root; the
// server takes all three from its own route. Order: swarm, user, access,
// swarm state, target, bridge.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, swarmID, agent, path string) {
	if swarmID == "" || agent == "" {
		http.NotFound(w, r)
		return
	}
	sw, ok, err := p.cfg.Swarms.SwarmByID(swarmID)
	if err != nil {
		http.Error(w, "cannot read swarm", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Отсутствующий рой не раскрываем даже фактом ошибки пути.
		http.NotFound(w, r)
		return
	}
	user, ok := p.cfg.CurrentUser(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !p.gate(w, r, user, sw, agent) {
		return
	}
	target, err := p.cfg.Resolve(sw.ID, agent)
	if err != nil {
		if errors.Is(err, ErrUnknownAgent) {
			http.NotFound(w, r)
			return
		}
		var apiErr *app.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable {
			http.Error(w, apiErr.Detail, http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "cannot find the agent terminal", http.StatusBadGateway)
		return
	}
	if isUpgrade(r) {
		p.serveUpgrade(w, r, user, sw, agent, target, path)
		return
	}
	p.servePlain(w, r, user, sw, agent, target, path)
}

// gate — авторизация до всякого соединения с агентом. Отказ и чужой
// рой — 404 без перечисления; не-active рой — 409 с состоянием.
// false — ответ записан.
func (p *Proxy) gate(w http.ResponseWriter, r *http.Request, user Identity, sw app.Swarm, agent string) bool {
	event := AuditEvent{Username: user.Username, SwarmID: sw.ID, Agent: agent, Method: r.Method}
	allowed, err := p.cfg.Authorize(user.ID, app.ActionManageSwarm, app.SwarmRes(sw.ID))
	switch {
	case err != nil:
		event.Reason = "authorization check failed"
		p.emit(event)
		http.Error(w, "authorization check failed", http.StatusInternalServerError)
		return false
	case !allowed:
		event.Reason = "not permitted"
		p.emit(event)
		http.NotFound(w, r)
		return false
	case sw.State != app.StateActive:
		event.Reason = "swarm is not active"
		p.emit(event)
		http.Error(w, "swarm is not active: "+string(sw.State), http.StatusConflict)
		return false
	}
	event.Allowed = true
	p.emit(event)
	return true
}

// ─── запрос к агенту ───────────────────────────────────────────────────────

// agentURL — адрес запроса к терминалу: путь внутри терминала и query
// исходного запроса. Хост не выбирает соединение: поток открывает Dial.
func (p *Proxy) agentURL(r *http.Request, path string) *url.URL {
	return &url.URL{Scheme: "http", Host: p.cfg.Host, Path: path, RawQuery: r.URL.RawQuery}
}

// isUpgrade — запрос смены протокола (WebSocket и совместимые).
func isUpgrade(r *http.Request) bool {
	for _, v := range r.Header.Values(headerConnection) {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return r.Header.Get(headerUpgrade) != ""
			}
		}
	}
	return false
}

// cleanOutbound подготавливает запрос к агенту: срезает hop-by-hop,
// учётные данные и cookie сессии brood, добавляет адрес клиента. Host и
// Origin — адрес терминала в агенте: ttyd сверяет их между собой, а
// Origin клиента сервер проверил до прокси.
func (p *Proxy) cleanOutbound(out *http.Request, orig *http.Request) {
	if ip, _, err := net.SplitHostPort(orig.RemoteAddr); err == nil {
		prior, ok := out.Header["X-Forwarded-For"]
		if ok && len(prior) > 0 {
			out.Header.Set("X-Forwarded-For", strings.Join(prior, ", ")+", "+ip)
		} else {
			out.Header.Set("X-Forwarded-For", ip)
		}
	}
	out.Header.Del("X-Forwarded-Host")
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	for _, h := range credentialHeaders {
		out.Header.Del(h)
	}
	if c := strings.Join(out.Header.Values("Cookie"), ";"); c != "" {
		filtered := strings.TrimSpace(filterCookies(c))
		if filtered == "" {
			out.Header.Del("Cookie")
		} else {
			out.Header.Set("Cookie", filtered)
		}
	}
	out.Host = p.cfg.Host
	out.Header.Set("Origin", "http://"+p.cfg.Host)
	// Ответ не для кэшей клиента прокси.
	out.Header.Del("Accept-Encoding")
	if p.cfg.RequestHook != nil {
		p.cfg.RequestHook(out, orig)
	}
}

// filterCookies убирает cookie сессии и CSRF brood, оставляя остальные.
func filterCookies(header string) string {
	var kept []string
	for _, part := range strings.Split(header, ";") {
		if part == "" {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(part, "=", 2)[0])
		if name == cookieSession || name == cookieCSRF {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, ";")
}

// ─── обычный HTTP ──────────────────────────────────────────────────────────

// servePlain — не-upgrade запрос: потолок тела, свой Transport
// с таймаутами, копия заголовков ответа без hop-by-hop.
func (p *Proxy) servePlain(w http.ResponseWriter, r *http.Request, user Identity, sw app.Swarm, agent string, target Target, path string) {
	ctx, cancel := context.WithCancel(r.Context())
	lc := &liveConn{user: user, swarm: sw, agent: agent, cancel: cancel, valid: p.targetCheck(r, target, p.cfg.Revalidate)}
	p.track(lc)
	defer p.untrack(lc)
	r.Body = http.MaxBytesReader(w, r.Body, p.cfg.MaxBodyBytes)
	out := r.Clone(ctx)
	out.URL = p.agentURL(r, path)
	p.cleanOutbound(out, r)
	// Соединение от имени прокси, не клиента.
	out.RequestURI = ""

	transport := p.transport(target)
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(out)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "the agent terminal is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if p.cfg.ResponseHook != nil {
		if err := p.cfg.ResponseHook(r, response); err != nil {
			http.Error(w, "the agent terminal response was rejected", http.StatusBadGateway)
			return
		}
	}

	for _, h := range hopHeaders {
		response.Header.Del(h)
	}
	for k, vs := range response.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

// ─── upgrade/WebSocket ─────────────────────────────────────────────────────

// serveUpgrade — WebSocket: ручной мост с хайджеком обеих сторон,
// регистрацией в реестре переоценки и двойным закрытием. Рукопожатие с
// агентом — запрос HTTP/1.1 по потоку Dial.
func (p *Proxy) serveUpgrade(w http.ResponseWriter, r *http.Request, user Identity, sw app.Swarm, agent string, target Target, path string) {
	outcome := UpgradeNotSubmitted
	var reservation UpgradeReservation
	if p.cfg.BeginUpgrade != nil {
		var err error
		reservation, err = p.cfg.BeginUpgrade(r, target)
		if err != nil {
			http.Error(w, "the terminal stream cannot open: the agent is busy or your access changed; open it again from the agents list", http.StatusConflict)
			return
		}
		if reservation.Finish != nil {
			defer func() { reservation.Finish(outcome) }()
		}
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection upgrade is not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, p.agentURL(r, path).String(), nil)
	if err != nil {
		writeRawResponse(clientBuf, http.StatusBadGateway, "invalid agent terminal address")
		return
	}
	for k, vs := range r.Header {
		upReq.Header[k] = vs
	}
	p.cleanOutbound(upReq, r)
	// Upgrade-семантика сохраняется, но цель выбрал сервер.
	upReq.Header.Set(headerConnection, "Upgrade")
	upReq.Header.Set(headerUpgrade, r.Header.Get(headerUpgrade))

	serverConn, err := p.dial(r.Context(), target)
	if err != nil {
		writeRawResponse(clientBuf, http.StatusBadGateway, "the agent terminal is unavailable")
		return
	}
	defer serverConn.Close()
	opening := p.targetCheck(r, target, p.cfg.Revalidate)
	valid := opening
	var established atomic.Bool
	if p.cfg.RevalidateStream != nil {
		stream := p.targetCheck(r, target, p.cfg.RevalidateStream)
		valid = func() bool {
			if established.Load() {
				return stream()
			}
			return opening == nil || opening()
		}
	}
	lc := &liveConn{user: user, swarm: sw, agent: agent, client: clientConn, server: serverConn, valid: valid}
	p.track(lc)
	defer p.untrack(lc)
	_ = serverConn.SetDeadline(time.Now().Add(p.cfg.ResponseHeaderTimeout))
	serverBuf := bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn))
	if err := func() error {
		if reservation.BeforeSubmit != nil {
			release, err := reservation.BeforeSubmit()
			if err != nil {
				return err
			}
			if release != nil {
				defer release()
			}
		}
		if err := r.Context().Err(); err != nil {
			return err
		}
		outcome = UpgradeSubmissionUnknown
		if err := upReq.Write(serverBuf); err != nil {
			return err
		}
		return serverBuf.Flush()
	}(); err != nil {
		writeRawResponse(clientBuf, http.StatusBadGateway, "cannot send the request to the agent terminal")
		return
	}
	upResp, err := http.ReadResponse(serverBuf.Reader, upReq)
	if err != nil {
		writeRawResponse(clientBuf, http.StatusBadGateway, "the agent terminal sent an invalid response")
		return
	}
	_ = serverConn.SetDeadline(time.Time{})
	if p.cfg.ResponseHook != nil {
		if err := p.cfg.ResponseHook(r, upResp); err != nil {
			upResp.Body.Close()
			writeRawResponse(clientBuf, http.StatusBadGateway, "the agent terminal response was rejected")
			return
		}
	}
	if upResp.StatusCode != http.StatusSwitchingProtocols {
		// Отказ агента уходит клиенту как есть: настоящий статус.
		if err := upResp.Write(clientBuf); err != nil {
			return
		}
		_ = clientBuf.Flush()
		if upResp.Body != nil {
			_ = upResp.Body.Close()
		}
		return
	}
	upResp.Body.Close()
	if p.cfg.RevalidateStream != nil && opening != nil && !opening() {
		writeRawResponse(clientBuf, http.StatusForbidden, "terminal access changed; open it again from the agents list")
		return
	}
	if err := writeUpgradeResponse(clientBuf, upResp); err != nil {
		return
	}

	established.Store(true)
	// Handshake прошёл; соединение уже в реестре переоценки.
	p.bridge(clientBuf, serverBuf, clientConn, serverConn)
}

// writeUpgradeResponse пишет клиенту 101 с заголовками агента.
func writeUpgradeResponse(clientBuf *bufio.ReadWriter, upResp *http.Response) error {
	upgrade := upResp.Header.Get(headerUpgrade)
	if upgrade == "" {
		upgrade = upResp.Request.Header.Get(headerUpgrade)
	}
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	fmt.Fprintf(&b, "%s: %s\r\n", headerUpgrade, upgrade)
	fmt.Fprintf(&b, "%s: Upgrade\r\n", headerConnection)
	for k, vs := range upResp.Header {
		if k == headerUpgrade || k == headerConnection {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := clientBuf.WriteString(b.String()); err != nil {
		return err
	}
	return clientBuf.Flush()
}

// bridge — двустороннее копирование; reader с буферами доедают байты,
// застрявшие после handshake, затем читают из соединений. Выход —
// когда закрылась любая сторона (defer-закрытия закрывают вторую).
func (p *Proxy) bridge(client *bufio.ReadWriter, server *bufio.ReadWriter, clientConn, serverConn net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(serverConn, client.Reader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, server.Reader)
		done <- struct{}{}
	}()
	<-done
}

// writeRawResponse пишет статусную строку и Close в захайджеченное
// соединение клиента; дальше соединение закроют defer-ы.
func writeRawResponse(w *bufio.ReadWriter, status int, reason string) error {
	text := http.StatusText(status)
	if text == "" {
		text = reason
		reason = ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		status, text, len(reason))
	b.WriteString(reason)
	_, err := w.WriteString(b.String())
	if err != nil {
		return err
	}
	return w.Flush()
}
