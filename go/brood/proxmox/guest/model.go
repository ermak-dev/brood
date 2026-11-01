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

// NewModelGateway uses only the supervisor's verified relay origin and token.
// Actor credentials and caller-selected origins never reach the server gateway.
func NewModelGateway(cfg relay.ClientConfig) (http.Handler, error) {
	target, err := url.Parse(cfg.URL)
	if err != nil || target.Scheme != "wss" || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || target.RawPath != "" || len(cfg.Token) != 43 || cfg.Generation <= 0 || cfg.ActorUID == "" || len(cfg.ActorUID) > 128 {
		return nil, errors.New("invalid model gateway configuration")
	}
	for _, r := range cfg.ActorUID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return nil, errors.New("invalid model gateway identity")
		}
	}
	generation := strconv.FormatInt(cfg.Generation, 10)
	guestPath := "/api/proxmox/guest/" + cfg.ActorUID + "/" + generation
	if target.Path != "" && target.Path != "/" && target.Path != guestPath {
		return nil, errors.New("invalid model gateway origin")
	}
	target.Scheme = "https"
	target.Path = ""
	target.RawPath = ""
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("model TLS roots unavailable")
	}
	if len(cfg.CAPEM) > 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(cfg.CAPEM) {
			return nil, errors.New("invalid model TLS certificate")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: target.Hostname()}
	proxy := &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/api/proxmox/llm/" + cfg.ActorUID + "/" + generation + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = target.Host
			pr.Out.Header = make(http.Header)
			for _, key := range []string{"Accept", "Content-Type", "Anthropic-Version", "Anthropic-Beta"} {
				for _, value := range pr.In.Header.Values(key) {
					pr.Out.Header.Add(key, value)
				}
			}
			pr.Out.Header.Set("Authorization", "Bearer "+cfg.Token)
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
		case "/v1/models":
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
			http.Error(w, "models request must not have a body", 400)
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

// ModelGatewayService owns the supervisor's fixed-loopback model endpoint.
type ModelGatewayService struct {
	server *http.Server
	done   chan struct{}
	once   sync.Once
	err    error
	stop   func() bool
}

// StartModelGateway binds before actor startup; a preexisting listener is never adopted.
func StartModelGateway(ctx context.Context, cfg relay.ClientConfig) (*ModelGatewayService, error) {
	handler, err := NewModelGateway(cfg)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", modelAddress)
	if err != nil {
		return nil, errors.New("model gateway listener unavailable")
	}
	service := &ModelGatewayService{done: make(chan struct{})}
	service.server = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	service.stop = context.AfterFunc(ctx, func() { service.server.Close() })
	go func() {
		defer close(service.done)
		err := service.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			service.err = errors.New("model gateway stopped unexpectedly")
		}
	}()
	return service, nil
}

func (s *ModelGatewayService) Wait() error { <-s.done; return s.err }
func (s *ModelGatewayService) Close() error {
	s.once.Do(func() { s.stop(); s.server.Close() })
	return s.Wait()
}
