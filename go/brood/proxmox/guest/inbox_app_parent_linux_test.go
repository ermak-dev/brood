//go:build linux

package guest

import "testing"

// BUG: An ordinary step marker cannot authorize the protected inbox app.
// HYPOTHESIS: The app needs root evidence of the exact held PID and start time.
func TestInboxAppRequiresTheExactSupervisorHeldParent(t *testing.T) {
	p := stepTestPolicy(t)
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	parent := inboxProcessIdentity{pid: 123, start: "456", uid: 1000, image: inboxGuestProcess}
	verify := func(actor inboxProcessIdentity) error {
		return verifyInboxAppParent(p.Run, p.Root, p.ActorGID, actor.pid, actor.start)
	}
	if err := verify(parent); err == nil {
		t.Fatal("ordinary step policy authorized an inbox app")
	}
	if err := p.writeInboxAppParent(parent); err != nil {
		t.Fatal(err)
	}
	if err := verify(parent); err != nil {
		t.Fatalf("exact held parent was refused: %v", err)
	}
	for _, changed := range []inboxProcessIdentity{{pid: 124, start: "456"}, {pid: 123, start: "457"}} {
		if err := verify(changed); err == nil {
			t.Fatalf("another process could use the held context: %+v", changed)
		}
	}
	if err := p.removeInboxAppParent(); err != nil {
		t.Fatal(err)
	}
	if err := verify(parent); err == nil {
		t.Fatal("the retired exact parent could still start the inbox app")
	}
}
