//go:build linux

package guest

import (
	"context"

	"brood.dev/go/brood/proxmox/relay"
)

// Control serializes fixed session mutations with sequenced terminal operations.
// An active writer must explicitly detach before another control path can mutate.
func (c *TerminalController) Control(ctx context.Context, run func() relay.SessionResponse) relay.SessionResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || ctx.Err() != nil {
		return terminalFailure("session connection is closed")
	}
	c.prune(c.now())
	for _, attachment := range c.attachments {
		if attachment.mode == "rw" && !attachment.closed && !attachment.invalid {
			return terminalFailure("close the active terminal writer before changing the session")
		}
	}
	return run()
}
