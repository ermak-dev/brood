//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"testing"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
)

func TestInboxHandoffRequiresTheExactLiveLaunchAndGrant(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	inbox, err := openScopedSupervisorInbox(ctx, base, deliverylog.Scope{ActorUID: "actor-1", Incarnation: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	body := []byte(`{"body":"exact retained instruction"}`)
	receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", body)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := NewInboxHandoff(ctx, inbox, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	grant := deliverylog.InboxTicket{Receipt: receipt, LaunchID: "launch-1", PermitID: "permit-1"}
	if work, err := handoff.Take(ctx, "launch-1"); !errors.Is(err, ErrInboxHandoffWaiting) || work.Payload != nil {
		t.Fatalf("consumer obtained work instead of a clean wait before authorization: %v", err)
	}
	wrong := grant
	wrong.LaunchID = "launch-2"
	if err := handoff.Offer(ctx, wrong); !errors.Is(err, ErrInboxHandoffUnavailable) {
		t.Fatalf("wrong launch armed a handoff: %v", err)
	}
	if err := handoff.Offer(ctx, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := handoff.Take(ctx, "launch-2"); !errors.Is(err, ErrInboxHandoffUnavailable) {
		t.Fatalf("wrong launch consumed work: %v", err)
	}
	if got, err := handoff.Take(ctx, "launch-1"); err != nil || got.Ticket != grant || string(got.Payload) != string(body) {
		t.Fatalf("authorized handoff changed exact instruction: %v", err)
	}
	if err := handoff.Offer(ctx, grant); !errors.Is(err, ErrInboxHandoffUnavailable) {
		t.Fatalf("lost reply could rearm the same grant: %v", err)
	}
	if _, err := handoff.Take(ctx, "launch-1"); !errors.Is(err, ErrInboxHandoffUnavailable) {
		t.Fatalf("released payload was presented twice: %v", err)
	}
	status, err := inbox.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest)
	if err != nil || status.State != deliverylog.InboxClaimed {
		t.Fatalf("handoff was not persisted or was mistaken for native receipt: %+v, %v", status, err)
	}
}

func TestInboxHandoffRevocationNeverReleasesArmedPayload(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	inbox, err := openScopedSupervisorInbox(ctx, base, deliverylog.Scope{ActorUID: "actor-1", Incarnation: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", []byte(`{"body":"before quiesce"}`))
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := NewInboxHandoff(ctx, inbox, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	grant := deliverylog.InboxTicket{Receipt: receipt, LaunchID: "launch-1", PermitID: "permit-1"}
	if err := handoff.Offer(ctx, grant); err != nil {
		t.Fatal(err)
	}
	handoff.Revoke()
	if work, err := handoff.Take(ctx, "launch-1"); !errors.Is(err, ErrInboxHandoffUnavailable) || work.Payload != nil {
		t.Fatalf("quiesced launch received a body: %v", err)
	}
	if err := handoff.Offer(ctx, grant); !errors.Is(err, ErrInboxHandoffUnavailable) {
		t.Fatalf("revocation could be rolled back by a delayed grant: %v", err)
	}
	status, err := inbox.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest)
	if err != nil || status.State != deliverylog.InboxQueued {
		t.Fatalf("revocation performed a delivery effect: %+v %v", status, err)
	}
}
