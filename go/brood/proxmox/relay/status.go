package relay

// StatusOperation is the read-only session operation behind the roster: what
// the session of an agent does and what its clone holds.
const StatusOperation = "status"

// Values of AgentStatus.Session.
const (
	SessionBusy        = "busy"
	SessionIdle        = "idle"
	SessionNeedsAction = "needs_action"
	SessionWaiting     = "waiting"
	SessionError       = "error"
	SessionOffline     = "offline"
	SessionUnknown     = "unknown"
)

// Values of AgentStatus.Clone.
const (
	CloneGit     = "git"
	CloneNone    = "none"
	CloneUnknown = "unknown"
)

// AgentStatus is the guest's answer to StatusOperation: facts only. The
// verdict on whether the agent is free is the server's.
type AgentStatus struct {
	// Session is one of the Session constants; Error is the code of the failed
	// turn when Session is SessionError.
	Session string `json:"session"`
	Error   string `json:"error,omitempty"`
	// Clone tells whether the workspace is a Git clone and whether Git
	// answered. Branch and the counts mean something only for CloneGit.
	Clone  string `json:"clone"`
	Branch string `json:"branch,omitempty"`
	// Uncommitted counts changed and untracked paths; Unpushed counts commits
	// of HEAD that no remote branch has.
	Uncommitted int `json:"uncommitted"`
	Unpushed    int `json:"unpushed"`
	// Model is the model saved in the settings of Claude Code; empty when none.
	Model string `json:"model,omitempty"`
}
