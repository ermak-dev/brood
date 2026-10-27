package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
)

// TLSFiles names projected router credentials. Each new connection reloads all
// files so certificate and trust-bundle rotation do not require a restart.
// No certificates are issued here; the installation supplies the identity.
type TLSFiles struct {
	CertFile   string
	KeyFile    string
	CAFile     string
	ServerName string
}

func (f *TLSFiles) config() (*tls.Config, error) {
	if f == nil || f.CertFile == "" || f.KeyFile == "" || f.CAFile == "" || f.ServerName == "" {
		return nil, errors.New("ATENET_CLIENT_CERT_FILE, ATENET_CLIENT_KEY_FILE, ATENET_CA_FILE and ATENET_SERVER_NAME are required")
	}
	cert, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load router client identity: %w", err)
	}
	ca, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load router CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("ATENET_CA_FILE contains no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, RootCAs: roots, ServerName: f.ServerName}, nil
}

func (p *Proxy) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	cfg, err := p.cfg.RouterTLS.config()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.DialTimeout)
	defer cancel()
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: p.cfg.DialTimeout}, Config: cfg}
	return dialer.DialContext(ctx, network, addr)
}
