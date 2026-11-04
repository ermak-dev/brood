//go:build linux

package guest

import (
	"reflect"
	"testing"
)

// FEATURE: Persistent PTY services must not hold the project cwd after Claude exits.
// SPEC: The host lives in home; only the fixed app wrapper selects the project
// and consumes project secrets. A browser attachment never launches an app.
func TestActorPTYKeepsPersistentServiceOutsideProject(t *testing.T) {
	cfg := actorPTYConfig("/opt/brood/bin/brood-guest")
	if cfg.Dir != "/workspace/home" || !reflect.DeepEqual(cfg.Command, []string{"/opt/brood/bin/brood-guest", "--brood-guest-app"}) {
		t.Fatalf("invalid PTY launch: %+v", cfg)
	}
	for _, arg := range actorArgs("/opt/brood/bin/brood-guest", true) {
		if arg == "--brood-guest-app" {
			t.Fatal("browser attach can create an app session")
		}
	}
}
