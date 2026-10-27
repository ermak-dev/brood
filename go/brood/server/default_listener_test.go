package server

import (
	"net"
	"testing"
)

func TestUnconfiguredServerListensOnlyOnLoopback(t *testing.T) {
	cfg, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("default plaintext listener is public: %q", cfg.Addr)
	}
}
