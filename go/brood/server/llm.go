package server

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

// publicLLMPath mirrors the old edge's explicit native-client routes, not a
// catch-all proxy. Canonical paths prevent traversal into management endpoints.
func publicLLMPath(p string) bool {
	if path.Clean(p) != strings.TrimSuffix(p, "/") {
		return false
	}
	for _, prefix := range []string{"/v1/", "/v1beta/", "/openai/v1/", "/backend-api/codex/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	switch p {
	case "/anthropic/callback", "/codex/callback", "/antigravity/callback", "/devin/callback", "/callback":
		return true
	}
	return false
}

func (s *Server) mountLLM(cfg Config) {
	public := llmProxy(cfg, false)
	management := llmProxy(cfg, true)
	for _, prefix := range []string{"/v1/", "/v1beta/", "/openai/v1/", "/backend-api/codex/"} {
		s.mux.Handle(prefix, public)
	}
	for _, p := range []string{"/anthropic/callback", "/codex/callback", "/antigravity/callback", "/devin/callback", "/callback"} {
		s.mux.Handle(p, public)
	}
	// The original panel preserves plugin-declared legacy extension/resource
	// URLs. They get the same admin gate, never the native-client bypass.
	for _, prefix := range []string{"/v8/management", "/v0/management", "/v0/resource"} {
		s.mux.Handle(prefix, s.adminLLM(management))
		s.mux.Handle(prefix+"/", s.adminLLM(management))
	}
}

func (s *Server) adminLLM(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := currentUser(w, r)
		if !ok {
			return
		}
		if !user.IsAdmin {
			writeDetail(w, 403, "administrator access required")
			return
		}
		// Session middleware already validates token and matching Origin; unlike
		// native CLI requests, management mutations must include Origin as well.
		if r.Method != http.MethodGet && r.Method != http.MethodHead || r.Header.Get("Upgrade") != "" {
			if r.Header.Get("Origin") == "" {
				writeDetail(w, 403, "same-origin request required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func llmProxy(cfg Config, management bool) http.Handler {
	if cfg.CLIProxyURL == "" || management && cfg.CLIProxyManagementKey == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeDetail(w, 503, "LLM backend is not configured: CLIPROXY_URL and (for management) CLIPROXY_MANAGEMENT_KEY are required")
		})
	}
	target, _ := url.Parse(cfg.CLIProxyURL)
	target.Path = ""
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Match the edge's Host behavior, including WebSocket Origin checks.
			pr.Out.Host = pr.In.Host
			for _, header := range []string{"Cookie", "X-Management-Key", "X-CSRFToken", "X-CSRF-Token"} {
				pr.Out.Header.Del(header)
			}
			if management {
				pr.Out.Header.Del("X-Api-Key")
				pr.Out.Header.Del("Api-Key")
				pr.Out.Header.Set("Authorization", "Bearer "+cfg.CLIProxyManagementKey)
			}
			// Do not trust browser-supplied forwarded identities or session metadata.
			pr.Out.Header.Del("Forwarded")
		},
		// Immediate flush supports SSE/chunked LLM streams. ReverseProxy also
		// handles HTTP/1.1 WebSocket upgrades without buffering their payload.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if management && resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return errors.New("management backend redirects are not allowed")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeDetail(w, 502, "LLM backend request failed")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeMux matches escaped segments; a native prefix containing encoded
		// traversal must not become a management path in a decoding backend.
		if !management && !publicLLMPath(r.URL.Path) {
			writeDetail(w, http.StatusNotFound, "not found")
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
