//go:build linux

package ptysession

import (
	"encoding/json"
	"testing"
)

// FEATURE: One supervisor-bound holder may launch at most one app process.
// SPEC: Ensure joins the live app, but cannot start another after that app exits.
func TestOneShotPTYHolderDoesNotRelaunchAfterExit(t *testing.T) {
	for _, once := range []bool{false, true} {
		name := "ordinary holder"
		if once {
			name = "one-shot holder"
		}
		t.Run(name, func(t *testing.T) {
			host, path := fixture(t, "ready")
			if once {
				// Missing fields are ignored on the RED baseline, so the same
				// behavioral test still exercises an ordinary real PTY.
				host.mu.Lock()
				err := json.Unmarshal([]byte(`{"OneShot":true}`), &host.config)
				host.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			start := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
			awaitScreen(t, path, start.Proof, "ready")
			again := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
			if again.Proof != start.Proof || !again.State.Running {
				t.Fatal("repeated ensure replaced a live PTY instead of joining it")
			}
			mustOK(t, rpc(t, path, Request{Action: "stop", Proof: start.Proof}))
			next := rpc(t, path, Request{Action: "ensure"})
			if once {
				if next.OK || next.Outcome != "refused" || next.State.Running || next.Error == "" {
					t.Fatalf("a consumed launch started a second real PTY: %+v", next)
				}
			} else if !next.OK || !next.State.Running || next.Proof == start.Proof {
				t.Fatalf("ordinary holder cannot restart its app: %+v", next)
			}
		})
	}
}
