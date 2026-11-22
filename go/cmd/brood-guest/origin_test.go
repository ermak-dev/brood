//go:build linux

package main

// FEATURE: Workspace origin fetched at boot (#147).
// SPEC: The bootstrap parser keeps the closed name list of main, so
// BROOD_GUEST_ORIGIN and BROOD_GUEST_ORIGIN_MODE are unknown variables. The
// root supervisor fetches the origin once from the server, after the project
// secrets cold fetch.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestConfigRefusesWorkspaceOriginVariables(t *testing.T) {
	env := validEnv(t)
	for _, extra := range [][]string{
		{"BROOD_GUEST_ORIGIN=https://git.example.org/group/repo.git", "BROOD_GUEST_ORIGIN_MODE=gateway"},
		{"BROOD_GUEST_ORIGIN=https://github.com/owner/repo"},
		{"BROOD_GUEST_ORIGIN_MODE=direct"},
		{"BROOD_GUEST_ORIGIN=", "BROOD_GUEST_ORIGIN_MODE="},
	} {
		_, err := parseConfig(append(append([]string{}, env...), extra...))
		if err == nil || err.Error() != "unknown guest configuration variable" {
			t.Fatalf("%v: %v", extra, err)
		}
	}
}

func TestSecretFailureStopsBeforeOriginFetch(t *testing.T) {
	d := dependencies{euid: func() int { return 0 }, secrets: func(context.Context, relay.ClientConfig) error { return errors.New("private-token") },
		workspace: func(context.Context, relay.ClientConfig) error {
			t.Fatal("origin fetched after a failed secrets cold fetch")
			return nil
		},
	}
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	err := dispatch(context.Background(), nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d)
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("unsafe or leaked failure: %v", err)
	}
}
