package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const managedLLMPrefix = "/api/proxmox/llm/"
const managedLLMBodyLimit = 8 << 20

// This bypass selects only the dedicated machine namespace, not native LLM
// routes. Dispatch directly: ServeMux would redirect noncanonical paths.
func publicManagedLLMPath(path string) bool { return strings.HasPrefix(path, managedLLMPrefix) }

func managedLLMRoute(r *http.Request) (uid string, generation int64, upstream string, ok bool) {
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || strings.Contains(r.URL.EscapedPath(), "%") {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, managedLLMPrefix), "/")
	if !publicManagedLLMPath(r.URL.Path) || (len(parts) != 4 && len(parts) != 5) || parts[2] != "v1" {
		return
	}
	uid = parts[0]
	if len(uid) == 0 || len(uid) > 128 {
		return
	}
	for _, c := range uid {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return
		}
	}
	var err error
	generation, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || generation <= 0 || parts[1] != strconv.FormatInt(generation, 10) {
		return
	}
	operation := strings.Join(parts[3:], "/")
	if operation != "models" && operation != "messages" && operation != "messages/count_tokens" {
		return
	}
	return uid, generation, "/v1/" + operation, true
}

func (s *Server) managedLLMHandler(cfg Config) http.Handler {
	if cfg.ActorLLMURL != "" {
		cfg.CLIProxyURL, cfg.CLIProxyAPIKey, cfg.CLIProxyCAPEM = cfg.ActorLLMURL, cfg.ActorLLMAPIKey, cfg.ActorLLMCAPEM
	}
	target, _ := url.Parse(cfg.CLIProxyURL)
	transport := cliProxyTransport(cfg)
	// Do not honor environment proxies: the only destination is the configured
	// CLIProxy origin. Header timeout is finite without limiting SSE idle gaps.
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxResponseHeaderBytes = 1 << 20
	transport.DisableKeepAlives = true
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			_, _, upstream, _ := managedLLMRoute(pr.In)
			pr.SetURL(target)
			pr.Out.URL.Path = upstream
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = ""
			pr.Out.URL.ForceQuery = false
			pr.Out.Host = target.Host
			// An allowlist also excludes future management/forwarding identity headers.
			pr.Out.Header = make(http.Header)
			for _, key := range []string{"Accept", "Content-Type", "Anthropic-Version", "Anthropic-Beta"} {
				for _, value := range pr.In.Header.Values(key) {
					pr.Out.Header.Add(key, value)
				}
			}
			pr.Out.Header.Set("Authorization", "Bearer "+cfg.CLIProxyAPIKey)
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return errors.New("model backend redirect denied")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeDetail(w, http.StatusBadGateway, "model backend request failed")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, generation, upstream, ok := managedLLMRoute(r)
		if !ok {
			writeDetail(w, http.StatusNotFound, "not found")
			return
		}
		expected := http.MethodPost
		if upstream == "/v1/models" {
			expected = http.MethodGet
		}
		if r.Method != expected {
			w.Header().Set("Allow", expected)
			writeDetail(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		credentials := r.Header.Values("Authorization")
		token := ""
		if len(credentials) == 1 && strings.HasPrefix(credentials[0], "Bearer ") {
			token = strings.TrimPrefix(credentials[0], "Bearer ")
		}
		// The existing HMAC token is 43 raw base64url characters; no shared secret
		// fallback, operator cookie or caller-selected upstream is accepted.
		if len(token) != 43 || !s.managed.authorizeGuest(uid, generation, token) {
			writeDetail(w, http.StatusUnauthorized, "guest authorization denied")
			return
		}
		if cfg.CLIProxyURL == "" || cfg.CLIProxyAPIKey == "" {
			writeDetail(w, http.StatusServiceUnavailable, "managed model access is not configured: CLIPROXY_URL and CLIPROXY_API_KEY are required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if !s.managed.authorizeGuest(uid, generation, token) {
						cancel()
						return
					}
				}
			}
		}()
		r = r.Clone(ctx)
		// Buffer only the bounded request, never the response. A read deadline also
		// covers chunked slow uploads before the upstream transport starts.
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, managedLLMBodyLimit))
		_ = controller.SetReadDeadline(time.Time{})
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeDetail(w, http.StatusRequestEntityTooLarge, "model request body too large")
			} else {
				writeDetail(w, http.StatusBadRequest, "model request body could not be read")
			}
			return
		}
		if upstream == "/v1/models" && len(body) != 0 {
			writeDetail(w, http.StatusBadRequest, "models request must not have a body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
		r.Trailer = nil
		proxy.ServeHTTP(w, r)
	})
}
