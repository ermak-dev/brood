package server

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
)

func cliProxyTransport(cfg Config) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if len(cfg.CLIProxyCAPEM) > 0 {
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(cfg.CLIProxyCAPEM)
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	return transport
}
