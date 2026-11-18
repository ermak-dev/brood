//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

var errClientHelloInspected = errors.New("TLS client hello inspected")

// Parser TLS не отправляет свои alerts клиенту и не завершает TLS за upstream.
// Все прочитанные байты затем уходят неизменными настоящему TLS endpoint.
type helloProbe struct {
	net.Conn
	reader io.Reader
	prefix bytes.Buffer
}

func (c *helloProbe) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if n > 0 {
		_, _ = c.prefix.Write(p[:n])
	}
	return n, err
}
func (*helloProbe) Write(p []byte) (int, error) { return len(p), nil }

func inspectClientHello(ctx context.Context, conn net.Conn, reader io.Reader, host string) ([]byte, error) {
	probe := &helloProbe{Conn: conn, reader: io.LimitReader(reader, 64<<10)}
	matched := false
	parser := tls.Server(probe, &tls.Config{MinVersion: tls.VersionTLS10, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		name, err := canonicalHost(hello.ServerName)
		matched = err == nil && name == host
		return nil, errClientHelloInspected
	}})
	err := parser.HandshakeContext(ctx)
	if !matched || !errors.Is(err, errClientHelloInspected) {
		return nil, errors.New("TLS client hello does not match approved destination")
	}
	return bytes.Clone(probe.prefix.Bytes()), nil
}

func (p *Proxy) connectTLS(w http.ResponseWriter, r *http.Request, address, host string) {
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
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = buffer.Flush(); err != nil {
		return
	}
	prefix, err := inspectClientHello(r.Context(), client, buffer, host)
	if err != nil {
		return
	}
	upstream, err := p.dial(r.Context(), "tcp", address)
	if err != nil {
		return
	}
	if !p.track(upstream) {
		return
	}
	defer p.forget(upstream)
	_ = upstream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = io.Copy(upstream, bytes.NewReader(prefix)); err != nil {
		return
	}
	forwardTunnel(r.Context(), client, upstream, buffer)
}
