//go:build linux

package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DialContext receives a validated literal IP, never a hostname.
type DialContext func(context.Context, string, string) (net.Conn, error)

// Proxy handles absolute HTTP requests and CONNECT without automatic redirects.
type Proxy struct {
	policy      *Policy
	dial        DialContext
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]bool
	slots       chan struct{}
}

// NewProxy requires an explicit policy and dialer.
func NewProxy(policy *Policy, dial DialContext) (*Proxy, error) {
	if policy == nil || dial == nil {
		return nil, errors.New("proxy requires policy and dialer")
	}
	return &Proxy{policy: policy, dial: dial, connections: map[net.Conn]bool{}, slots: make(chan struct{}, 64)}, nil
}
func controls(s string) bool {
	for _, c := range s {
		if c < 32 || c == 127 {
			return true
		}
	}
	return false
}
func (p *Proxy) target(r *http.Request) (string, string, error) {
	u := r.URL
	if u == nil || u.User != nil || u.Opaque != "" || u.Fragment != "" || controls(r.RequestURI) || controls(r.Host) {
		return "", "", errors.New("invalid proxy request")
	}
	for k, values := range r.Header {
		if controls(k) {
			return "", "", errors.New("invalid header")
		}
		for _, v := range values {
			if controls(v) {
				return "", "", errors.New("invalid header")
			}
		}
	}
	decoded, err := url.PathUnescape(u.String())
	if err != nil || controls(decoded) {
		return "", "", errors.New("invalid URL")
	}
	port := u.Port()
	host := u.Hostname()
	if r.Method == http.MethodConnect {
		var err error
		host, port, err = net.SplitHostPort(r.Host)
		if err != nil || u.Host != r.Host || u.Path != "" || u.RawQuery != "" {
			return "", "", errors.New("invalid CONNECT authority")
		}
	} else {
		if u.Scheme != "http" || !u.IsAbs() || u.Host != r.Host {
			return "", "", errors.New("absolute HTTP URL required")
		}
		if strings.Contains(u.Host, ":") {
			var err error
			host, port, err = net.SplitHostPort(u.Host)
			if err != nil || port == "" {
				return "", "", errors.New("invalid HTTP authority")
			}
		} else {
			port = "80"
		}
	}
	if port != "80" && port != "443" {
		return "", "", errors.New("port is not approved")
	}
	h, err := canonicalHost(host)
	if err != nil {
		return "", "", err
	}
	return h, port, nil
}
func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = c.Close()
		return false
	}
	p.connections[c] = true
	return true
}
func (p *Proxy) forget(c net.Conn) {
	_ = c.Close()
	p.mu.Lock()
	delete(p.connections, c)
	p.mu.Unlock()
}

// Close also closes hijacked CONNECT streams, which http.Server.Close does not own.
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for c := range p.connections {
		_ = c.Close()
	}
	return nil
}

// ServeHTTP never lets the transport resolve an unchecked hostname.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "proxy capacity reached", http.StatusServiceUnavailable)
		return
	}
	host, port, err := p.target(r)
	if err != nil {
		http.Error(w, "proxy request denied", http.StatusForbidden)
		return
	}
	ips, err := p.policy.Resolve(r.Context(), host)
	if err != nil {
		http.Error(w, "proxy destination denied", http.StatusForbidden)
		return
	}
	address := net.JoinHostPort(ips[0].String(), port)
	if r.Method == http.MethodConnect {
		p.connect(w, r, address)
		return
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 15 * time.Second, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return p.dial(ctx, "tcp", address) }}
	defer transport.CloseIdleConnections()
	request := r.Clone(r.Context())
	request.RequestURI = ""
	request.Header = r.Header.Clone()
	stripHopHeaders(request.Header)
	response, err := transport.RoundTrip(request)
	if err != nil {
		http.Error(w, "proxy upstream failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	stripHopHeaders(response.Header)
	for k, values := range response.Header {
		for _, value := range values {
			w.Header().Add(k, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
func stripHopHeaders(h http.Header) {
	for _, token := range strings.Split(h.Get("Connection"), ",") {
		h.Del(strings.TrimSpace(token))
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, address string) {
	upstream, err := p.dial(r.Context(), "tcp", address)
	if err != nil {
		http.Error(w, "proxy upstream failed", http.StatusBadGateway)
		return
	}
	if !p.track(upstream) {
		return
	}
	defer p.forget(upstream)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT requires a connection", http.StatusBadGateway)
		return
	}
	client, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if !p.track(client) {
		return
	}
	defer p.forget(client)
	_ = client.SetDeadline(time.Now().Add(12 * time.Hour))
	_ = upstream.SetDeadline(time.Now().Add(12 * time.Hour))
	if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buffer); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	select {
	case <-done:
	case <-r.Context().Done():
	}
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
