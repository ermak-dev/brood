//go:build linux

package main

import (
	"context"
	"errors"
	"testing"
)

// FEATURE: ttyd uses the fixed raw PTY attach target (#162).
// SPEC: Dispatch precedes bootstrap env access, invokes only the attach helper,
// rejects extra arguments, and propagates the helper's failure without fallback.
func TestPTYAttachDispatchBeforeBootstrapEnvironment(t *testing.T) {
	expected := errors.New("attachment refused")
	called := 0
	d := dependencies{attach: func(context.Context) error { called++; return expected }}
	env := func() []string { t.Fatal("attach read supervisor configuration"); return nil }
	err := dispatch(context.Background(), []string{"--brood-guest-attach"}, env, d)
	if !errors.Is(err, expected) || called != 1 {
		t.Fatalf("attach dispatch calls=%d error=%v", called, err)
	}
	if err := dispatch(context.Background(), []string{"--brood-guest-attach", "extra"}, env, d); err == nil || called != 1 {
		t.Fatal("extra attach arguments accepted")
	}
}
