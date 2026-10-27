package server

import (
	"net/http"
)

// browserProtection keeps upgrades inside session CSRF validation. Browsers
// cannot set a WebSocket header, so they may supply the session CSRF token as
// a query parameter; it is removed before routing and never sent to actors.
func (s *Server) browserProtection() http.Handler {
	normal := s.auth.Middleware(s.mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrade := r.Header.Get("Upgrade") != ""
		if !upgrade {
			normal.ServeHTTP(w, r)
			return
		}
		clone := r.Clone(r.Context())
		urlCopy := *r.URL
		clone.URL = &urlCopy
		query := clone.URL.Query()
		if clone.Header.Get("X-CSRFToken") == "" {
			clone.Header.Set("X-CSRFToken", query.Get("csrf"))
		}
		query.Del("csrf")
		clone.URL.RawQuery = query.Encode()
		method := clone.Method
		clone.Method = http.MethodPost
		s.auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, checked *http.Request) {
			checked.Method = method
			s.mux.ServeHTTP(w, checked)
		})).ServeHTTP(w, clone)
	})
}
