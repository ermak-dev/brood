package command

// NeedsSwarm reports whether an operator command requires a selected swarm.
// This is a client preflight check, not a server authorization boundary.
func NeedsSwarm(name string) bool {
	switch name {
	case "models", "profile", "diagnose", "server_diagnose", "project_add", "project_limit", "node", "node_drain", "node_up":
		return false
	default:
		return true
	}
}
