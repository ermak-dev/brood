package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"brood.dev/go/brood/gitorigin"
)

const (
	managedGitPrefix = "/api/proxmox/git/"
	// Пределы контракта #146: тело upload-pack, тело receive-pack, весь запрос
	// и число одновременных запросов одного актора.
	gitUploadPackLimit  = 64 << 20
	gitReceivePackLimit = 1 << 30
	gitGatewayTimeout   = 30 * time.Minute
	gitGatewayPerActor  = 4
	// gitGatewayUserAgent — фиксированный агент сервера: агент гостя не передаётся.
	gitGatewayUserAgent = "git/brood-gateway"
)

// gitGateway — серверное посредничество Git для managed-акторов (#146):
// гость не достаёт до частного Git установки, сервер подставляет учётные
// данные remote только в upstream-запрос. Тела запросов и ответов потоковые,
// не журналируются; redirect не выполняется. Тот же транспорт разрешает
// ветку проекта при запуске роя (#228) в пределах headTimeout.
type gitGateway struct {
	remotes      gitRemotes
	transport    *http.Transport
	proxy        *httputil.ReverseProxy
	timeout      time.Duration
	uploadLimit  int64
	receiveLimit int64
	perActor     int
	headTimeout  time.Duration
	mu           sync.Mutex
	active       map[string]int
}

func (*gitGateway) String() string               { return "server.gitGateway{redacted}" }
func (g *gitGateway) GoString() string           { return g.String() }
func (g *gitGateway) Format(f fmt.State, _ rune) { redactFormat(f, g) }

// gitTarget — проверенный upstream одного запроса; передаётся в Rewrite через
// контекст, чтобы URL и credential выбирал только сервер.
type gitTarget struct {
	upstream   url.URL
	credential gitCredential
}

// fmt печатает неэкспортируемые поля без их String: скрываем всю структуру.
func (gitTarget) String() string               { return "server.gitTarget{redacted}" }
func (t gitTarget) GoString() string           { return t.String() }
func (t gitTarget) Format(f fmt.State, _ rune) { redactFormat(f, t) }

type gitTargetKey struct{}

var errGitRedirect = errors.New("git upstream redirect denied")

// errGitAddressDenied — адрес upstream вне допустимых для шлюза.
var errGitAddressDenied = errors.New("git upstream address denied")

// gitTranslatedPrefixes — IPv6 с вложенным IPv4 для шлюза трансляции или
// туннеля: NAT64 (64:ff9b::/96, local-use 64:ff9b:1::/48), 6to4 (2002::/16),
// Teredo (2001::/32), IPv4-compatible (::/96) и IPv4-translated
// (::ffff:0:0:0/96). Шлюз доставит такой адрес на вложенный IPv4, в том числе
// loopback и metadata, мимо проверок IPv4. Отказ — для всего префикса, без
// разбора вложенного адреса: local-use NAT64 кладёт IPv4 в позицию, зависящую
// от длины префикса сети, которую сервер не знает. В сети только с IPv6 и
// DNS64 upstream нужен собственный IPv6-адрес: синтезированный NAT64 отвергается.
var gitTranslatedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::ffff:0:0:0/96"),
}

// gitDialControl — граница SSRF после разрешения имени: upstream, заданный
// через API (#173), не попадает на loopback, link-local (включая metadata
// 169.254.169.254), unspecified, multicast, broadcast, reserved адреса и IPv6
// с вложенным IPv4 (gitTranslatedPrefixes). Частные адреса LAN допустимы: Git
// установки обычно там; те же правила deploy применяет к remotes установки при
// выкладке.
func gitDialControl(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return errGitAddressDenied
	}
	ip := addrPort.Addr().Unmap()
	if !ip.IsGlobalUnicast() {
		return errGitAddressDenied
	}
	if ip.Is4() {
		if first := ip.As4()[0]; first == 0 || first >= 240 {
			return errGitAddressDenied
		}
	}
	for _, prefix := range gitTranslatedPrefixes {
		if prefix.Contains(ip) {
			return errGitAddressDenied
		}
	}
	return nil
}

func newGitGateway(remotes gitRemotes) *gitGateway {
	if remotes == nil {
		remotes = gitRemotes{}
	}
	g := &gitGateway{remotes: remotes, timeout: gitGatewayTimeout, uploadLimit: gitUploadPackLimit, receiveLimit: gitReceivePackLimit, perActor: gitGatewayPerActor, headTimeout: gitHeadTimeout, active: map[string]int{}}
	// Без прокси окружения: единственные адресаты — настроенные remotes.
	g.transport = &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: gitDialControl}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    30 * time.Second,
		ResponseHeaderTimeout:  10 * time.Minute,
		MaxResponseHeaderBytes: 1 << 20,
		DisableCompression:     true,
		IdleConnTimeout:        90 * time.Second,
	}
	g.proxy = &httputil.ReverseProxy{
		Transport: g.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := pr.In.Context().Value(gitTargetKey{}).(gitTarget)
			upstream := target.upstream
			pr.Out.URL = &upstream
			pr.Out.Host = upstream.Host
			// Allowlist: Authorization гостя, cookies и forwarded identity не уходят.
			pr.Out.Header = make(http.Header)
			for _, key := range []string{"Content-Type", "Accept", "Content-Encoding", "Git-Protocol"} {
				for _, value := range pr.In.Header.Values(key) {
					pr.Out.Header.Add(key, value)
				}
			}
			pr.Out.Header.Set("User-Agent", gitGatewayUserAgent)
			if target.credential.configured() {
				pr.Out.SetBasicAuth(target.credential.username, target.credential.token)
			}
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return errGitRedirect
			}
			header := make(http.Header)
			for _, key := range []string{"Content-Type", "Content-Encoding", "Cache-Control"} {
				for _, value := range resp.Header.Values(key) {
					header.Add(key, value)
				}
			}
			resp.Header = header
			resp.Trailer = nil
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				writeDetail(w, http.StatusRequestEntityTooLarge, "Git request body too large")
			case errors.Is(r.Context().Err(), context.DeadlineExceeded):
				writeDetail(w, http.StatusGatewayTimeout, "Git upstream request timed out")
			default:
				writeDetail(w, http.StatusBadGateway, "Git upstream request failed")
			}
		},
		// Ошибки копирования тел могут содержать адрес upstream: не журналируем.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return g
}

// acquire занимает слот актора; false — превышен предел одновременных запросов.
func (g *gitGateway) acquire(uid string) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[uid] >= g.perActor {
		return nil, false
	}
	g.active[uid]++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.active[uid]--; g.active[uid] <= 0 {
			delete(g.active, uid)
		}
	}, true
}

func publicManagedGitPath(path string) bool { return strings.HasPrefix(path, managedGitPrefix) }

// gitRoute — разобранная операция smart HTTP с её ограничениями.
type gitRoute struct {
	uid         string
	generation  int64
	operation   string
	query       string
	method      string
	contentType string
}

// managedGitRoute принимает только info/refs (GET, query ровно service=
// git-upload-pack или git-receive-pack), git-upload-pack и git-receive-pack
// (POST без query). Percent-encoding, fragment и неканоническое поколение —
// отказ: ServeMux не участвует, путь не очищается.
func managedGitRoute(r *http.Request) (gitRoute, bool) {
	var route gitRoute
	if r.URL.RawPath != "" || r.URL.ForceQuery || r.URL.Fragment != "" || strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.RawQuery, "%") {
		return route, false
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, managedGitPrefix), "/", 3)
	if !publicManagedGitPath(r.URL.Path) || len(parts) != 3 || !projectSecretActorUID.MatchString(parts[0]) {
		return route, false
	}
	generation, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != parts[1] {
		return route, false
	}
	route.uid, route.generation, route.operation, route.query = parts[0], generation, parts[2], r.URL.RawQuery
	switch route.operation {
	case "info/refs":
		route.method = http.MethodGet
		return route, r.URL.RawQuery == "service=git-upload-pack" || r.URL.RawQuery == "service=git-receive-pack"
	case "git-upload-pack", "git-receive-pack":
		route.method = http.MethodPost
		route.contentType = "application/x-" + route.operation + "-request"
		return route, r.URL.RawQuery == ""
	}
	return route, false
}

// serveManagedGit — /api/proxmox/git/{uid}/{generation}/{op}: машинный маршрут
// вне cookie/CSRF middleware. Авторизация — та же, что у шлюза моделей и
// секретов проекта, плюс режим origin актора = gateway на каждом запросе.
func (s *Server) serveManagedGit(w http.ResponseWriter, r *http.Request) {
	route, ok := managedGitRoute(r)
	if !ok {
		writeDetail(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != route.method {
		w.Header().Set("Allow", route.method)
		writeDetail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	credentials := r.Header.Values("Authorization")
	token := ""
	if len(credentials) == 1 && strings.HasPrefix(credentials[0], "Bearer ") {
		token = strings.TrimPrefix(credentials[0], "Bearer ")
	}
	if s.managed == nil || s.managed.hub == nil || s.managed.registry == nil || len(s.managed.registry.relaySecret) < 32 || len(token) != 43 || !s.managed.authorizeGuest(route.uid, route.generation, token) {
		writeDetail(w, http.StatusUnauthorized, "guest authorization denied")
		return
	}
	timeout := s.git.timeout
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	target, status, detail := s.gitGatewayTarget(ctx, route, token, true)
	if status != 0 {
		writeDetail(w, status, detail)
		return
	}
	release, ok := s.git.acquire(route.uid)
	if !ok {
		writeDetail(w, http.StatusTooManyRequests, "too many concurrent Git requests for this actor")
		return
	}
	defer release()
	if route.method == http.MethodGet {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			writeDetail(w, http.StatusBadRequest, "Git ref advertisement request must not have a body")
			return
		}
		r.Body = http.NoBody
	} else {
		if types := r.Header.Values("Content-Type"); len(types) != 1 || types[0] != route.contentType {
			writeDetail(w, http.StatusUnsupportedMediaType, "Git request requires content type "+route.contentType)
			return
		}
		limit := s.git.uploadLimit
		if route.operation == "git-receive-pack" {
			limit = s.git.receiveLimit
		}
		if r.ContentLength > limit {
			writeDetail(w, http.StatusRequestEntityTooLarge, "Git request body too large")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	// Остановка, reset, смена repository проекта, remote или ссылки декларации,
	// ротация и удаление Git credential отзывают и уже открытый поток:
	// проверка раз в секунду отменяет upstream.
	watcher := make(chan struct{})
	go func() {
		defer close(watcher)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.managed.authorizeGuest(route.uid, route.generation, token) {
					cancel()
					return
				}
				if current, status, _ := s.gitGatewayTarget(ctx, route, token, false); status != 0 || current.upstream.String() != target.upstream.String() || current.credential != target.credential {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-watcher
	}()
	// Соединение живёт чуть дольше контекста: отказ по сроку успевает уйти гостю.
	deadline := time.Now().Add(timeout + 10*time.Second)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	s.git.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, gitTargetKey{}, target)))
}

// gitGatewayTarget повторяет проверку привязки secrets runtime: актор, рой,
// atespace и (при namespace) native namespace, затем требует, чтобы origin
// актора сейчас был в режиме gateway. Credential берётся только из текущего
// Git-доступа проекта: привязанного проектного credential или remote установки.
func (s *Server) gitGatewayTarget(ctx context.Context, route gitRoute, token string, namespace bool) (gitTarget, int, string) {
	denied := func() (gitTarget, int, string) {
		return gitTarget{}, http.StatusUnauthorized, "guest authorization denied"
	}
	actor, err := s.store.GetManagedActor(ctx, route.uid)
	if err != nil || actor.Incarnation != route.generation {
		return denied()
	}
	swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil || swarm.State != "active" || swarm.AtespaceUID == nil || *swarm.AtespaceUID != actor.AtespaceUID {
		return denied()
	}
	if namespace {
		if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
			return gitTarget{}, http.StatusServiceUnavailable, "Git gateway unavailable"
		}
		if !s.managed.authorizeGuest(route.uid, route.generation, token) {
			return denied()
		}
	}
	access, err := s.projectGitAccess(ctx, swarm.ProjectID)
	if err != nil {
		return gitTarget{}, http.StatusServiceUnavailable, "Git gateway unavailable"
	}
	if access.mode(actor.Origin) != gitorigin.ModeGateway {
		return gitTarget{}, http.StatusForbidden, "Git gateway is not enabled for this actor's origin"
	}
	upstream, err := url.Parse(actor.Origin + "/" + route.operation)
	if err != nil {
		return gitTarget{}, http.StatusServiceUnavailable, "Git gateway unavailable"
	}
	upstream.RawQuery = route.query
	return gitTarget{upstream: *upstream, credential: access.credential}, 0, ""
}
