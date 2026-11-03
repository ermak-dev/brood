//go:build linux

package guest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

const gitAddress = "127.0.0.1:8318"

// Пределы тел совпадают с серверным gateway: upload-pack несёт только wants/haves,
// receive-pack — пушимый pack.
const gitUploadLimit = 64 << 20
const gitReceiveLimit = 1 << 30
const gitConcurrency = 4

type gitRoute struct {
	op, method string
	limit      int64 // 0: тело запрещено
}

// gitRoutes are the only actor paths; dumb HTTP and other repositories are absent.
var gitRoutes = map[string]gitRoute{
	"/origin.git/info/refs":        {"info/refs", http.MethodGet, 0},
	"/origin.git/git-upload-pack":  {"git-upload-pack", http.MethodPost, gitUploadLimit},
	"/origin.git/git-receive-pack": {"git-receive-pack", http.MethodPost, gitReceiveLimit},
}

var gitRequestHeaders = []string{"Accept", "Content-Type", "Content-Encoding", "Git-Protocol"}
var gitResponseHeaders = []string{"Content-Type", "Content-Encoding", "Cache-Control"}

// NewGitGateway proxies the actor's smart-HTTP Git to the server's machine route
// for its own origin. The actor never sees the token nor selects the repository.
func NewGitGateway(cfg relay.ClientConfig) (http.Handler, error) {
	return newGitGateway(cfg, gitAddress)
}

func newGitGateway(cfg relay.ClientConfig, address string) (http.Handler, error) {
	route, err := newServerRoute(cfg)
	if err != nil {
		return nil, err
	}
	// После полной отправки receive-pack сервер индексирует pack до заголовков.
	transport := route.transport(10 * time.Minute)
	transport.DisableCompression = true
	base := route.path("/api/proxmox/git/")
	proxy := &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(route.origin)
			pr.Out.URL.Path = base + "/" + gitRoutes[pr.In.URL.Path].op
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = route.origin.Host
			pr.Out.Header = allowedHeaders(pr.In.Header, gitRequestHeaders)
			pr.Out.Header.Set("Authorization", "Bearer "+route.token)
		},
		ModifyResponse: func(r *http.Response) error {
			if r.StatusCode >= 300 && r.StatusCode < 400 {
				return errors.New("git redirect denied")
			}
			r.Header = allowedHeaders(r.Header, gitResponseHeaders)
			// 401 заставил бы Git актора спрашивать пароль, который шлюз всё равно отбросит.
			if r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusProxyAuthRequired {
				r.StatusCode, r.Status = http.StatusForbidden, ""
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "git gateway unavailable", http.StatusBadGateway)
		},
	}
	slots := make(chan struct{}, gitConcurrency)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != address || r.Header.Get("Origin") != "" || r.URL.RawPath != "" || r.URL.ForceQuery || r.URL.User != nil {
			http.Error(w, "git request denied", http.StatusForbidden)
			return
		}
		endpoint, ok := gitRoutes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != endpoint.method {
			http.Error(w, "git method denied", http.StatusMethodNotAllowed)
			return
		}
		if !gitQuery(endpoint.op, r.URL.RawQuery) {
			http.Error(w, "git request denied", http.StatusForbidden)
			return
		}
		if endpoint.limit == 0 && r.ContentLength != 0 {
			http.Error(w, "git discovery must not have a body", http.StatusBadRequest)
			return
		}
		if r.ContentLength > endpoint.limit {
			http.Error(w, "git request body exceeds limit", http.StatusRequestEntityTooLarge)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "git request limit reached", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		r = r.Clone(ctx)
		if endpoint.limit == 0 {
			r.Body = http.NoBody
		} else {
			// Поток без буфера: предел проверяется по мере чтения.
			r.Body = http.MaxBytesReader(w, r.Body, endpoint.limit)
		}
		proxy.ServeHTTP(w, r)
	}), nil
}

func gitQuery(op, query string) bool {
	if op == "info/refs" {
		return query == "service=git-upload-pack" || query == "service=git-receive-pack"
	}
	return query == ""
}

func allowedHeaders(source http.Header, keys []string) http.Header {
	result := make(http.Header)
	for _, key := range keys {
		for _, value := range source.Values(key) {
			result.Add(key, value)
		}
	}
	return result
}

// StartGitGateway binds before actor startup; a preexisting listener is never adopted.
// Request deadlines follow the 30-minute server bound, not the model gateway's.
func StartGitGateway(ctx context.Context, cfg relay.ClientConfig) (*GatewayService, error) {
	handler, err := NewGitGateway(cfg)
	if err != nil {
		return nil, err
	}
	return startGatewayService(ctx, gitAddress, &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Minute, WriteTimeout: 30 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}, "Git gateway")
}
