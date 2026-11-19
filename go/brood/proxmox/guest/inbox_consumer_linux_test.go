//go:build linux

package guest

import "testing"

func TestInboxConsumerRequiresCurrentActorAndFixedProcessChain(t *testing.T) {
	actor := inboxProcessIdentity{pid: 10, parent: 1, start: "100", uid: 1000, image: inboxGuestProcess}
	claude := inboxProcessIdentity{pid: 20, parent: 10, start: "110", uid: 1000, image: inboxClaudeProcess}
	helper := inboxProcessIdentity{pid: 30, parent: 20, start: "120", uid: 1000, image: inboxGuestProcess}
	if !matchesInboxConsumer(actor, claude, helper, actor) {
		t.Fatal("fixed helper launched by the tracked actor's Claude was refused")
	}
	cases := []struct {
		actor, claude, helper inboxProcessIdentity
	}{
		{inboxProcessIdentity{pid: 10, parent: 1, start: "130", uid: 1000, image: inboxGuestProcess}, claude, helper},
		{actor, inboxProcessIdentity{pid: 20, parent: 10, start: "110", uid: 1000, image: inboxUnknownProcess}, helper},
		{actor, claude, inboxProcessIdentity{pid: 30, parent: 21, start: "120", uid: 1000, image: inboxGuestProcess}},
		{actor, claude, inboxProcessIdentity{pid: 30, parent: 20, start: "120", uid: 999, image: inboxGuestProcess}},
	}
	for index, c := range cases {
		if matchesInboxConsumer(c.actor, c.claude, c.helper, actor) {
			t.Fatalf("accepted another process chain: case %d", index)
		}
	}
}
