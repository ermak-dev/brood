//go:build linux

package main

import (
	"context"
	"testing"
)

func TestMCPDispatchNeverReadsSupervisorCredentials(t *testing.T) {
	called := false
	d := dependencies{mcp: func(context.Context) error { called = true; return nil }}
	if err := dispatch(context.Background(), []string{"mcp"}, func() []string { t.Fatal("MCP read machine credentials"); return nil }, d); err != nil || !called {
		t.Fatal(err, called)
	}
}
