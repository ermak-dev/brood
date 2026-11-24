package proxy

// FEATURE: Relay-compatible provider dial cancellation.
// SPEC: Opening is bounded, but a successful connection retains its context
// until Close. Relay streams must not die at the end of the Dial call.

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestProviderDialContextLifetime(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	var lifetime context.Context
	p := New(Config{DialTimeout: 20 * time.Millisecond, Dial: func(ctx context.Context, _ Target) (net.Conn, error) { lifetime = ctx; return a, nil }})
	conn, err := p.dial(context.Background(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-lifetime.Done():
		t.Fatal("successful dial canceled live relay context")
	default:
	}
	// An opening timeout must be disarmed, not become a terminal lifetime limit.
	timer := time.NewTimer(40 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if lifetime.Err() != nil {
		t.Fatal("dial timeout killed established stream")
	}
	conn.Close()
	select {
	case <-lifetime.Done():
	case <-time.After(time.Second):
		t.Fatal("connection Close did not release lifetime context")
	}
}
func TestProviderDialOpeningBounded(t *testing.T) {
	p := New(Config{DialTimeout: 10 * time.Millisecond, Dial: func(ctx context.Context, _ Target) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }})
	if _, err := p.dial(context.Background(), Target{}); err == nil {
		t.Fatal("unbounded provider opening")
	}
}
