package command

// NeedsSwarm reports whether an operator command requires a selected swarm.
// This is a client preflight check, not a server authorization boundary.
func NeedsSwarm(name string) bool {
	switch name {
	case "agents", "send", "tail", "slash", "artifact_publish", "artifact_get", "add", "rm", "start", "stop", "restart", "reset":
		return true
	default:
		return false
	}
}
