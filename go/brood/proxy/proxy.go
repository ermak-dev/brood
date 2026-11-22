// Пакет proxy проксирует HTTP и WebSocket терминала агента.
// Цель соединения вычисляет сервер из роя и агента; пользователь
// не управляет адресом или заголовками выбора агента. Права проверяются до соединения и
// переоцениваются по интервалу: отозванный доступ закрывает живые
// соединения. Библиотека молчит: события уходят в аудит-колбэк,
// отказы — статусами с причиной строкой.
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
	"time"

	"brood.dev/go/brood/app"
)

// Дефолты; переопределяются конфигурацией.
const (
	// DefaultRecheckInterval — период переоценки прав живых соединений.
	DefaultRecheckInterval = 60 * time.Second
	// DefaultMaxBodyBytes — потолок тела не-WebSocket запроса.
	DefaultMaxBodyBytes int64 = 4 << 20
	// DefaultDialTimeout — потолок установления соединения с актором.
	DefaultDialTimeout = 10 * time.Second
	// DefaultResponseHeaderTimeout — потолок ожидания статусной строки
	// upstream для не-WebSocket запроса.
	DefaultResponseHeaderTimeout = 60 * time.Second
)

// Заголовки, которыми выбирают актора на стороне ateapi-router. Их
// клиент прислать не может: прокси стирает их и задаёт сам.
const (
	// HeaderActor — имя актора-цели.
	HeaderActor = "X-Actor"
	// HeaderAtespace — atespace роя цели.
	HeaderAtespace = "X-Atespace"
	// HeaderActorTarget — составной селектор цели ateapi-router.
	HeaderActorTarget = "X-Actor-Target"
)

// Имена cookie сессии и CSRF brood: вверх не идут никогда.
const (
	cookieSession = "session"
	cookieCSRF    = "csrf"
)

// strippedHeaders — заголовки выбора цели: клиент не задаёт адрес
// актора ни напрямую, ни составным селектором.
var strippedHeaders = []string{HeaderActor, HeaderAtespace, HeaderActorTarget, "Ate-Target-Actor", "Authorization", "X-CSRFToken"}

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

// ErrUnknownActor — резолвер не знает актора в atespace роя.
var ErrUnknownActor = errors.New("actor is not routable in its swarm")

// Identity — пользователь, прошедший аутентификацию. Минимальный
// интерфейс к слою auth: идентификатор для app.Can и имя для аудита.
type Identity struct {
	ID       string
	Username string
}

// CurrentUser возвращает пользователя аутентифицированного запроса;
// ok=false — аноним (сессии нет). Реализация — обёртка над middleware
// auth (UserFromContext); сам прокси аутентификацией не занимается.
type CurrentUser func(ctx context.Context) (Identity, bool)

// Authorizer — проверка прав; реализация — app.Service.Can, действие
// app.ActionManageSwarm, ресурс app.SwarmRes. Ошибка — внутренний
// сбой, не запрет.
type Authorizer func(userID string, action app.Action, res app.Resource) (bool, error)

// SwarmLookup — чтение роя по ID; реализация — app.Store.
type SwarmLookup interface {
	SwarmByID(id string) (app.Swarm, bool, error)
}

// UpstreamResolver находит URL агента по рою и имени.
type UpstreamResolver interface {
	ActorUpstream(swarmID, actor string) (*url.URL, error)
}

// AuditEvent — факт решения прокси: кто, куда, чем и разрешено ли.
// Поля плоские для slog.Attr.
type AuditEvent struct {
	Username string
	SwarmID  string
	Actor    string
	Method   string
	Allowed  bool
	// Reason — контекст решения (not permitted, swarm is not active,
	// access revoked и т.п.); пуст у разрешённых запросов.
	Reason string
}

// Audit — приёмник событий аудита; nil — аудит выключен.
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

// Config — зависимости и настройки прокси.
type Config struct {
	// DialContext optionally routes plain HTTP and upgrades to a managed provider.
	// HTTPS always retains the native router's verified TLS transport.
	DialContext func(context.Context, string, string) (net.Conn, error)
	// RequestHook applies server-controlled policy after outbound sanitization.
	RequestHook func(out, original *http.Request)
	// ResponseHook applies server-controlled policy before any headers are sent.
	ResponseHook func(original *http.Request, response *http.Response) error
	// BeginUpgrade reserves provider-specific mutation authority before upgrade I/O.
	BeginUpgrade func(*http.Request) (UpgradeReservation, error)
	// RouterTLS supplies projected mTLS credentials for HTTPS ingress.
	RouterTLS *TLSFiles
	// Revalidate checks the original session on long-lived requests.
	Revalidate func(*http.Request) bool
	// UpstreamValid fences the resolved provider incarnation on long-lived requests.
	UpstreamValid func(*url.URL) bool
	// Swarms — чтение роёв по ID.
	Swarms SwarmLookup
	// Authorize — гейт прав до соединения.
	Authorize Authorizer
	// CurrentUser — текущий пользователь из контекста запроса.
	CurrentUser CurrentUser
	// Upstream — резолвер адреса актора.
	Upstream UpstreamResolver
	// Audit — события решений (nil допустим).
	Audit Audit
	// RecheckInterval — период переоценки прав живых соединений;
	// 0 — DefaultRecheckInterval.
	RecheckInterval time.Duration
	// MaxBodyBytes — потолок тела не-WS запроса; 0 — DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// DialTimeout — потолок соединения с актором; 0 — DefaultDialTimeout.
	DialTimeout time.Duration
	// ResponseHeaderTimeout — потолок статусной строки upstream; 0 —
	// DefaultResponseHeaderTimeout.
	ResponseHeaderTimeout time.Duration
	// Logger — slog; nil — slog.Default().
	Logger *slog.Logger
}

// Proxy — HTTP handler прокси и реестр живых соединений.
type Proxy struct {
	cfg    Config
	logger *slog.Logger
	once   sync.Once
	rt     http.RoundTripper

	mu    sync.Mutex
	conns map[*liveConn]struct{}
}

// liveConn — живое upgrade-соединение с отслеживаемым доступом.
// close зовут и переоценка, и завершение копирования: флаг страхует
// от двойного закрытия, параллельные Close TCPConn безопасны.
type liveConn struct {
	user   Identity
	swarm  app.Swarm
	actor  string
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

// New собирает прокси из зависимостей. Swarms, Authorize, CurrentUser
// и Upstream обязательны; нулевые интервалы заменяются дефолтами.
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

// transport — разделяемый RoundTripper с таймаутами конфигурации.
func (p *Proxy) transport() http.RoundTripper {
	p.once.Do(func() {
		p.rt = &http.Transport{
			DialContext:    p.dialContext,
			DialTLSContext: p.dialTLS,
			// Projected identity rotation must affect the next request, not
			// wait for a pooled TLS connection to expire.
			DisableKeepAlives:     p.cfg.RouterTLS != nil,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: p.cfg.ResponseHeaderTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		}
	})
	return p.rt
}

type providerConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *providerConn) Close() error { c.cancel(); return c.Conn.Close() }

func (p *Proxy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if p.cfg.DialContext == nil {
		return (&net.Dialer{Timeout: p.cfg.DialTimeout, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	}
	// Relay Dial binds context to the returned stream, not just opening.
	// Disarm the opening timer on success and release context on Close.
	lifetime, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(p.cfg.DialTimeout, cancel)
	conn, err := p.cfg.DialContext(lifetime, network, address)
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
	return &providerConn{Conn: conn, cancel: cancel}, nil
}

// Close terminates active requests and idle upstream connections.
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
	if rt, ok := p.transport().(*http.Transport); ok {
		rt.CloseIdleConnections()
	}
}

// Start запускает фоновую переоценку прав живых соединений; стоп —
// отмена контекста. Без вызова прокси работает, но отзывает доступ
// только по явному RecheckNow.
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

// RecheckNow переоценивает права всех живых соединений и закрывает
// отозванные. Публично: сервер может звать по событию (блокировка,
// смена членства), не дожидаясь тикера.
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
		Username: lc.user.Username, SwarmID: lc.swarm.ID, Actor: lc.actor,
		Allowed: false, Reason: reason,
	})
}

// track регистрирует живое соединение.
func (p *Proxy) sessionCheck(r *http.Request) func() bool {
	if p.cfg.Revalidate == nil {
		return nil
	}
	return func() bool { return p.cfg.Revalidate(r) }
}

func (p *Proxy) targetCheck(r *http.Request, base *url.URL) func() bool {
	session := p.sessionCheck(r)
	if session == nil && p.cfg.UpstreamValid == nil {
		return nil
	}
	return func() bool {
		return (session == nil || session()) && (p.cfg.UpstreamValid == nil || p.cfg.UpstreamValid(base))
	}
}

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
		"actor", e.Actor,
		"method", e.Method,
		"allowed", e.Allowed,
		"reason", e.Reason)
}

// Mount регистрирует маршруты прокси в mux сервера:
// /swarms/{id}/actors/{name}[/путь актора]. {rest...} склеивается
// обратно actorSubPath-ом, mux-паттерн только отбирает запросы.
func (p *Proxy) Mount(mux *http.ServeMux) {
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		mux.Handle(method+" /swarms/{swarm}/actors/{actor}", p)
		mux.Handle(method+" /swarms/{swarm}/actors/{actor}/{rest...}", p)
	}
}

// ServeHTTP — /swarms/{id}/actors/{name}[/путь актора]. Порядок:
// рой -> пользователь -> права -> состояние роя -> резолв -> мост.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	swarmID := r.PathValue("swarm")
	actor := r.PathValue("actor")
	if swarmID == "" || actor == "" {
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
	if !p.gate(w, r, user, sw, actor) {
		return
	}
	base, err := p.cfg.Upstream.ActorUpstream(sw.ID, actor)
	if err != nil {
		if errors.Is(err, ErrUnknownActor) {
			http.NotFound(w, r)
			return
		}
		var apiErr *app.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable {
			http.Error(w, apiErr.Detail, http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "cannot resolve actor upstream", http.StatusBadGateway)
		return
	}
	if isUpgrade(r) {
		p.serveUpgrade(w, r, user, sw, actor, base)
		return
	}
	p.servePlain(w, r, user, sw, actor, base)
}

// gate — авторизация до всякого соединения с upstream. Отказ и чужой
// рой — 404 без перечисления (как Python-панель прячет чужие
// проекты); не-active рой — 409 с состоянием. false — ответ записан.
func (p *Proxy) gate(w http.ResponseWriter, r *http.Request, user Identity, sw app.Swarm, actor string) bool {
	event := AuditEvent{Username: user.Username, SwarmID: sw.ID, Actor: actor, Method: r.Method}
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

// ─── адрес цели ────────────────────────────────────────────────────────────

// targetURL строит адрес upstream: путь после имени актора
// сохраняется, query тоже.
func targetURL(base *url.URL, r *http.Request) *url.URL {
	out := *base
	out.Path = singleJoiningSlash(base.Path, actorSubPath(r))
	out.RawQuery = r.URL.RawQuery
	return &out
}

// actorSubPath — часть пути после /swarms/{id}/actors/{name}, без
// самого имени актора; "/" для пути, равного имени.
func actorSubPath(r *http.Request) string {
	const marker = "/actors/"
	idx := strings.Index(r.URL.Path, marker)
	if idx < 0 {
		return "/"
	}
	rest := r.URL.Path[idx+len(marker):] // "{actor}[/путь]"
	if slash := strings.Index(rest, "/"); slash >= 0 {
		return rest[slash:]
	}
	return "/"
}

// singleJoiningSlash соединяет пути ровно одной косой чертой.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
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

// cleanOutbound подготавливает запрос к upstream: срезает hop-by-hop,
// заголовки выбора цели и cookie сессии brood, добавляет адрес клиента
// и серверные заголовки цели. Host уже задан самим запросом.
func (p *Proxy) cleanOutbound(out *http.Request, orig *http.Request, sw app.Swarm, actor string) {
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
	for _, h := range strippedHeaders {
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
	// Цель задаёт только сервер.
	out.Header.Set(HeaderActor, actor)
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
func (p *Proxy) servePlain(w http.ResponseWriter, r *http.Request, user Identity, sw app.Swarm, actor string, base *url.URL) {
	ctx, cancel := context.WithCancel(r.Context())
	lc := &liveConn{user: user, swarm: sw, actor: actor, cancel: cancel, valid: p.targetCheck(r, base)}
	p.track(lc)
	defer p.untrack(lc)
	r.Body = http.MaxBytesReader(w, r.Body, p.cfg.MaxBodyBytes)
	out := r.Clone(ctx)
	out.URL = targetURL(base, r)
	out.Host = r.Host
	p.cleanOutbound(out, r, sw, actor)
	// Соединение от имени прокси, не клиента.
	out.RequestURI = ""

	response, err := p.transport().RoundTrip(out)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "actor upstream is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if p.cfg.ResponseHook != nil {
		if err := p.cfg.ResponseHook(r, response); err != nil {
			http.Error(w, "actor response rejected", http.StatusBadGateway)
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
// регистрацией в реестре переоценки и двойным закрытием.
func (p *Proxy) serveUpgrade(w http.ResponseWriter, r *http.Request, user Identity, sw app.Swarm, actor string, base *url.URL) {
	outcome := UpgradeNotSubmitted
	var reservation UpgradeReservation
	if p.cfg.BeginUpgrade != nil {
		var err error
		reservation, err = p.cfg.BeginUpgrade(r)
		if err != nil {
			http.Error(w, "actor stream conflicts with active work or current authority", http.StatusConflict)
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

	target := targetURL(base, r)
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		writeRawResponse(clientBuf, http.StatusBadGateway, "invalid upstream address")
		return
	}
	for k, vs := range r.Header {
		upReq.Header[k] = vs
	}
	upReq.Host = r.Host
	p.cleanOutbound(upReq, r, sw, actor)
	// Upgrade-семантика сохраняется, но цель выбрал сервер.
	upReq.Header.Set(headerConnection, "Upgrade")
	upReq.Header.Set(headerUpgrade, r.Header.Get(headerUpgrade))

	var serverConn net.Conn
	if target.Scheme == "https" {
		serverConn, err = p.dialTLS(r.Context(), "tcp", target.Host)
	} else {
		serverConn, err = p.dialContext(r.Context(), "tcp", target.Host)
	}
	if err != nil {
		writeRawResponse(clientBuf, http.StatusBadGateway, "actor upstream is unavailable")
		return
	}
	defer serverConn.Close()
	lc := &liveConn{user: user, swarm: sw, actor: actor, client: clientConn, server: serverConn, valid: p.targetCheck(r, base)}
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
		writeRawResponse(clientBuf, http.StatusBadGateway, "cannot submit to actor upstream")
		return
	}
	upResp, err := http.ReadResponse(serverBuf.Reader, upReq)
	if err != nil {
		writeRawResponse(clientBuf, http.StatusBadGateway, "invalid actor upstream response")
		return
	}
	_ = serverConn.SetDeadline(time.Time{})
	if p.cfg.ResponseHook != nil {
		if err := p.cfg.ResponseHook(r, upResp); err != nil {
			upResp.Body.Close()
			writeRawResponse(clientBuf, http.StatusBadGateway, "actor response rejected")
			return
		}
	}
	if upResp.StatusCode != http.StatusSwitchingProtocols {
		// Отказ upstream уходит клиенту как есть: настоящий статус.
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

	if err := writeUpgradeResponse(clientBuf, upResp); err != nil {
		return
	}

	// Handshake прошёл; соединение уже в реестре переоценки.
	p.bridge(clientBuf, serverBuf, clientConn, serverConn)
}

// writeUpgradeResponse пишет клиенту 101 с заголовками upstream.
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
