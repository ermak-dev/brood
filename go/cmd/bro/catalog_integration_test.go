package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FEATURE: Catalog utilities retain global scope and self-contained help.
// SPEC: Global commands do not require a swarm; help never authenticates or executes.
func TestCatalogCommandHelp(t *testing.T) {
	var out, diagnostic bytes.Buffer
	code := runWithIO(context.Background(), []string{"send", "--help"}, strings.NewReader(""), &out, &diagnostic)
	if code != 0 || !strings.Contains(out.String(), "wait-seconds") {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), diagnostic.String())
	}
}

func TestDiagnoseIsOfflineAndReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("BROOD_EXECUTION_MODE", "operator")
	var out, diagnostic bytes.Buffer
	code := runWithIO(context.Background(), []string{"diagnose"}, strings.NewReader(""), &out, &diagnostic)
	if code != 0 || !strings.Contains(out.String(), "unconfigured") {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), diagnostic.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("offline diagnosis mutated session directory: %v", err)
	}
}

func TestGlobalCommandScope(t *testing.T) {
	for _, name := range []string{"models", "profile", "diagnose", "server_diagnose", "project_add", "project_limit", "node", "node_drain", "node_up"} {
		if operatorCommandNeedsSwarm(name) {
			t.Errorf("global command requires swarm: %s", name)
		}
	}
	for _, name := range []string{"send", "add", "tail", "dispatch", "landing", "session_history"} {
		if !operatorCommandNeedsSwarm(name) {
			t.Errorf("actor command lost required scope: %s", name)
		}
	}
}

func TestGuestModeDoesNotUseOperatorWhoami(t *testing.T) {
	var out, diagnostic bytes.Buffer
	code := runWithIO(context.Background(), []string{"--guest", "whoami"}, strings.NewReader(""), &out, &diagnostic)
	if code == 0 || !strings.Contains(diagnostic.String(), "guest") {
		t.Fatalf("code=%d err=%s", code, diagnostic.String())
	}
}
