//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

const modelAddress = "127.0.0.1:8317"
const modelBodyLimit = 8 << 20
const rolePromptPath = "/_brood/role-prompt"

// serverRoute is the supervisor's verified relay origin and scoped token. Every
// loopback gateway derives its upstream from it, never from the actor request.
type serverRoute struct {
	origin     *url.URL // https://<relay host>, without path
	uid, token string
	generation string
	tls        *tls.Config
}

func newServerRoute(cfg relay.ClientConfig) (serverRoute, error) {
	target, err := url.Parse(cfg.URL)
	if err != nil || target.Scheme != "wss" || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || target.RawPath != "" || len(cfg.Token) != 43 || cfg.Generation <= 0 || cfg.ActorUID == "" || len(cfg.ActorUID) > 128 {
		return serverRoute{}, errors.New("invalid gateway configuration")
	}
	for _, r := range cfg.ActorUID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return serverRoute{}, errors.New("invalid gateway identity")
		}
	}
	generation := strconv.FormatInt(cfg.Generation, 10)
	guestPath := "/api/proxmox/guest/" + cfg.ActorUID + "/" + generation
	if target.Path != "" && target.Path != "/" && target.Path != guestPath {
		return serverRoute{}, errors.New("invalid gateway origin")
	}
	target.Scheme = "https"
	target.Path = ""
	target.RawPath = ""
	roots, err := x509.SystemCertPool()
	if err != nil {
		return serverRoute{}, errors.New("gateway TLS roots unavailable")
	}
	if len(cfg.CAPEM) > 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(cfg.CAPEM) {
			return serverRoute{}, errors.New("invalid gateway TLS certificate")
		}
	}
	return serverRoute{origin: target, uid: cfg.ActorUID, token: cfg.Token, generation: generation, tls: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: target.Hostname()}}, nil
}

// path is the server machine route of this actor generation under prefix.
func (r serverRoute) path(prefix string) string { return prefix + r.uid + "/" + r.generation }

func (r serverRoute) transport(headerTimeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ResponseHeaderTimeout = headerTimeout
	transport.TLSClientConfig = r.tls.Clone()
	return transport
}

// NewModelGateway uses only the supervisor's verified relay origin and token.
// Actor credentials and caller-selected origins never reach the server gateway.
func NewModelGateway(cfg relay.ClientConfig) (http.Handler, error) {
	route, err := newServerRoute(cfg)
	if err != nil {
		return nil, err
	}
	target := route.origin
	transport := route.transport(30 * time.Second)
	base := route.path("/api/proxmox/llm/")
	proxy := &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = base + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = target.Host
			pr.Out.Header = make(http.Header)
			if pr.In.URL.Path == rolePromptPath {
				pr.Out.URL.Path = route.path("/api/proxmox/role-prompt/")
			} else {
				for _, key := range []string{"Accept", "Content-Type", "Anthropic-Version", "Anthropic-Beta"} {
					for _, value := range pr.In.Header.Values(key) {
						pr.Out.Header.Add(key, value)
					}
				}
			}
			pr.Out.Header.Set("Authorization", "Bearer "+route.token)
		},
		ModifyResponse: func(r *http.Response) error {
			r.Header.Del("Set-Cookie")
			if r.StatusCode >= 300 && r.StatusCode < 400 {
				return errors.New("model redirect denied")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "model gateway unavailable", 502) },
	}
	slots := make(chan struct{}, 8)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != modelAddress || r.Header.Get("Origin") != "" || r.URL.RawPath != "" || !modelQuery(r.URL.Path, r.URL.RawQuery) || r.URL.ForceQuery || r.URL.User != nil {
			http.Error(w, "model request denied", 403)
			return
		}
		expected := "POST"
		switch r.URL.Path {
		case "/v1/models", rolePromptPath:
			expected = "GET"
		case "/v1/messages", "/v1/messages/count_tokens":
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != expected {
			http.Error(w, "model method denied", 405)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "model request limit reached", 503)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, modelBodyLimit))
		if err != nil {
			http.Error(w, "model request body exceeds limit or is unavailable", 413)
			return
		}
		if expected == "GET" && len(body) != 0 {
			http.Error(w, "GET request must not have a body", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		r = r.Clone(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
		r.Trailer = nil
		proxy.ServeHTTP(w, r)
	}), nil
}

func modelQuery(path, query string) bool {
	if query == "" {
		return true
	}
	if path == "/v1/messages" || path == "/v1/messages/count_tokens" {
		return query == "beta=true"
	}
	limit, ok := strings.CutPrefix(query, "limit=")
	if path != "/v1/models" || !ok {
		return false
	}
	n, err := strconv.Atoi(limit)
	return err == nil && n >= 1 && n <= 1000 && strconv.Itoa(n) == limit
}

// GatewayService owns one of the supervisor's fixed-loopback gateways.
type GatewayService struct {
	server *http.Server
	done   chan struct{}
	once   sync.Once
	err    error
	stop   func() bool
}

// StartModelGateway binds before actor startup; a preexisting listener is never adopted.
func StartModelGateway(ctx context.Context, cfg relay.ClientConfig) (*GatewayService, error) {
	handler, err := NewModelGateway(cfg)
	if err != nil {
		return nil, err
	}
	return startGatewayService(ctx, modelAddress, &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}, "model gateway")
}

func startGatewayService(ctx context.Context, address string, server *http.Server, name string) (*GatewayService, error) {
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, errors.New(name + " listener unavailable")
	}
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	service := &GatewayService{server: server, done: make(chan struct{})}
	service.stop = context.AfterFunc(ctx, func() { service.server.Close() })
	go func() {
		defer close(service.done)
		err := service.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			service.err = errors.New(name + " stopped unexpectedly")
		}
	}()
	return service, nil
}

func (s *GatewayService) Wait() error { <-s.done; return s.err }
func (s *GatewayService) Close() error {
	s.once.Do(func() { s.stop(); s.server.Close() })
	return s.Wait()
}
