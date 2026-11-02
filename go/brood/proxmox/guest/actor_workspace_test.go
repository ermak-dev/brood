//go:build linux

package guest

import "testing"

// FEATURE: Persistent ttyd does not keep the project workspace busy after Claude stops.
// SPEC: The service starts outside the clone and tmux explicitly places Claude in the project.
func TestActorTmuxExplicitProjectDirectory(t *testing.T) {
	args := actorArgs()
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) && args[i+1] == "/workspace/project" {
			return
		}
	}
	t.Fatal("tmux inherits persistent ttyd cwd instead of selecting the project directory")
}
