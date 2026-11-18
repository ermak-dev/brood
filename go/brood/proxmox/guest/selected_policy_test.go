//go:build linux

package guest

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/egress"
)

func TestSelectedPolicyChecksProtocolBeforeDNS(t *testing.T) {
	lookups, dials := 0, 0
	p, err := NewNetworkPolicy([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{"example.com"}, Ports: []int{443}}}, func(context.Context, string) ([]net.IP, error) {
		lookups++
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, _ := NewProxy(p, func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected dial")
	})
	defer proxy.Close()
	r := httptest.NewRequest("GET", "http://example.com:443/path", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	if w.Code != 403 || lookups != 0 || dials != 0 {
		t.Fatalf("forbidden HTTP reached DNS or dial: status=%d lookups=%d dials=%d", w.Code, lookups, dials)
	}
}

func TestSelectedHTTPPortCarriesProjectToolCredential(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com:8080" || r.Header.Get("Authorization") != "Bearer synthetic-project-token" {
			t.Error("project tool request changed")
		}
		fmt.Fprint(w, "tool-ok")
	}))
	defer backend.Close()
	p, err := NewNetworkPolicy([]egress.Rule{{Protocol: egress.HTTP, Hostnames: []string{"example.com"}, Ports: []int{8080}}}, publicLookup)
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	proxy, _ := NewProxy(p, func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "8.8.8.8:8080" {
			t.Errorf("unexpected checked address %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(backend.URL, "http://"))
	})
	defer proxy.Close()
	r := httptest.NewRequest("GET", "http://example.com:8080/path", nil)
	r.Header.Set("Authorization", "Bearer synthetic-project-token")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "tool-ok" || dials != 1 {
		t.Fatalf("selected HTTP denied: %d %q dials=%d", w.Code, w.Body.String(), dials)
	}
}

func TestSelectedPolicyOfflineAndPrivateDNSRemainClosed(t *testing.T) {
	for _, private := range []bool{false, true} {
		var rules []egress.Rule
		if private {
			rules = []egress.Rule{{Protocol: egress.HTTP, Hostnames: []string{"example.com"}, Ports: []int{8080}}}
		}
		p, err := NewNetworkPolicy(rules, func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.1.2.3")}, nil })
		if err != nil {
			t.Fatal(err)
		}
		proxy, _ := NewProxy(p, func(context.Context, string, string) (net.Conn, error) {
			t.Error("private or offline destination reached dial")
			return nil, errors.New("unexpected dial")
		})
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com:8080/", nil))
		proxy.Close()
		if w.Code != 403 {
			t.Fatalf("unsafe destination status=%d", w.Code)
		}
	}
}

func selectedTLSFixture(t *testing.T) (*Proxy, *httptest.Server, *x509.CertPool, *atomic.Int32) {
	t.Helper()
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "tls-tool-ok") }))
	t.Cleanup(backend.Close)
	roots := x509.NewCertPool()
	roots.AddCert(backend.Certificate())
	p, err := NewNetworkPolicy([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{"example.com"}, Ports: []int{8443}}}, publicLookup)
	if err != nil {
		t.Fatal(err)
	}
	dials := &atomic.Int32{}
	proxy, err := NewProxy(p, func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if address != "8.8.8.8:8443" {
			t.Errorf("unchecked TLS destination %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, backend.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.Close() })
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return proxy, server, roots, dials
}

func TestSelectedTLSPassthroughPreservesEndToEndTLS(t *testing.T) {
	_, server, roots, dials := selectedTLSFixture(t)
	proxyURL, _ := url.Parse(server.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("https://example.com:8443/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != "tls-tool-ok" || response.TLS == nil || len(response.TLS.VerifiedChains) == 0 || dials.Load() != 1 {
		t.Fatalf("TLS passthrough failed: status=%d body=%q dials=%d", response.StatusCode, body, dials.Load())
	}
}

func TestSelectedTLSRejectsForeignOrMissingSNIBeforeDial(t *testing.T) {
	for _, sni := range []string{"foreign.example", "127.0.0.1"} {
		t.Run(sni, func(t *testing.T) {
			_, server, roots, dials := selectedTLSFixture(t)
			proxyURL, _ := url.Parse(server.URL)
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: sni}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			response, err := client.Get("https://example.com:8443/")
			if response != nil {
				response.Body.Close()
			}
			if err == nil || dials.Load() != 0 {
				t.Fatalf("unapproved SNI reached destination: error=%v dials=%d", err, dials.Load())
			}
		})
	}
}

func TestSelectedTLSRejectsPlaintextTunnelBeforeDial(t *testing.T) {
	_, server, _, dials := selectedTLSFixture(t)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "CONNECT example.com:8443 HTTP/1.1\r\nHost: example.com:8443\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("CONNECT setup refused before client hello: %d", response.StatusCode)
	}
	fmt.Fprint(conn, "GET / HTTP/1.0\r\n\r\n")
	data, err := io.ReadAll(reader)
	if err != nil || len(data) != 0 || dials.Load() != 0 {
		t.Fatalf("plaintext tunneled: bytes=%d error=%v dials=%d", len(data), err, dials.Load())
	}
}
