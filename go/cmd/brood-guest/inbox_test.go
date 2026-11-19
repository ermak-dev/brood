//go:build linux

package main

import (
	"context"
	"testing"
)

func TestInboxHelperDispatchAcceptsNoActorSelectedArguments(t *testing.T) {
	calls := 0
	d := dependencies{inbox: func(context.Context) error { calls++; return nil }}
	env := func() []string { t.Fatal("inbox helper read supervisor settings"); return nil }
	if err := dispatch(context.Background(), []string{"--brood-guest-inbox"}, env, d); err != nil || calls != 1 {
		t.Fatalf("fixed inbox helper dispatch failed: %v", err)
	}
	if err := dispatch(context.Background(), []string{"--brood-guest-inbox", "/other.sock"}, env, d); err == nil || calls != 1 {
		t.Fatal("actor selected an inbox socket or command")
	}
}
