package clientapi

import (
	"crypto/x509"
	"fmt"
)

// newCAPool собирает пул доверенных CA из PEM; системные корни остаются.
func newCAPool(pemBytes []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no valid certificates in CA file")
	}
	return pool, nil
}
