//go:build linux

package guest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// BUG: A second supervisor could adopt and then delete the first firewall.
// SPEC: The first nft command must acquire exclusive ownership atomically.
func TestFirewallCreationRequiresExclusiveOwnership(t *testing.T) {
	existing := true
	removed := false
	command := func(_ context.Context, _ string, _ []string, script string) error {
		first := strings.SplitN(script, "\n", 2)[0]
		if first == "create table inet brood_guest" && existing {
			return errors.New("table exists")
		}
		if strings.Contains(script, "delete table") {
			removed = true
			existing = false
		}
		return nil
	}
	err := applyRules(context.Background(), nftRules(), command)
	if err == nil {
		t.Fatal("second supervisor accepted an existing firewall")
	}
	if removed || !existing {
		t.Fatal("existing runtime lost its enforcement")
	}
}
