package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
)

type signedStepInboxRelay struct {
	durableInstructionRelay
	receiver string
	open     func(context.Context, relay.InboxRequest) (net.Conn, error)
}

func (h signedStepInboxRelay) InboxReceiverID(uid string, incarnation int64) string {
	if h.DeliveryJournalID(uid, incarnation) == "" {
		return ""
	}
	return h.receiver
}

func (h signedStepInboxRelay) DialInbox(ctx context.Context, request relay.InboxRequest) (net.Conn, error) {
	return h.open(ctx, request)
}

// Exercise the real dispatcher and its manager check, not a Store call with an
// allow-all callback. The first refused pass must not affect the new source.
func TestDispatcherCommitsSignedInboxOnFreshSecondPass(t *testing.T) {
	d, _ := durableDispatchServer(t)
	d.reply = func(guestAttest) (relay.SessionResponse, error) {
		return relay.SessionResponse{Outcome: "failed", Text: "fixture refused before delivery"}, nil
	}
	d.start("deliver")
	if d.last().State != "refused" {
		t.Fatal("fixture did not refuse its first pass")
	}
	d.stop()

	const commit = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	const launch = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	secret := d.srv.managed.registry.relaySecret
	hub := signedStepInboxRelay{durableInstructionRelay: d.srv.managed.hub.(durableInstructionRelay), receiver: strings.Repeat("b", 64)}
	d.reply = func(call guestAttest) (relay.SessionResponse, error) {
		response, err := signedAttestation(t, workspaceAttestationKey(secret), call, call.Generation)
		if err != nil {
			return response, err
		}
		workspace, err := attest.DecodeAttestation([]byte(response.Text))
		if err != nil {
			return response, err
		}
		claim := attest.InboxLaunchClaim{Claim: workspace.Claim, LaunchID: launch, ReceiverID: hub.receiver}
		signature, err := attest.SignInboxLaunch(attest.InboxLaunchKey(guestToken(secret, call.UID, call.Generation)), claim)
		if err == nil {
			response.InboxLaunch = &attest.InboxLaunchProof{InboxLaunchClaim: claim, Signature: signature}
		}
		return response, err
	}
	var operations []relay.InboxOperation
	hub.open = func(ctx context.Context, request relay.InboxRequest) (net.Conn, error) {
		operations = append(operations, request.Operation)
		s := d.srv.store
		intent, err := s.GetActorDeliveryForSource(ctx, d.swarm, "step", "2")
		if err != nil {
			t.Error("relay opened before a committed intent:", err)
			return nil, err
		}
		selected, err := s.ActorDeliveryInboxStatus(ctx, intent.ID)
		if err != nil || selected.Target.ReceiverID != hub.receiver || intent.Input.Commit != commit || !d.leaseReleased(d.worker) {
			t.Error("relay opened without committed exact workspace, receiver, and released lease:", err)
		}
		if request.Operation == relay.InboxAdmit {
			return inboxResponseStream(t, request, relay.InboxCustodyRecorded), nil
		}
		if request.Operation != relay.InboxDispatch || request.Grant == nil || request.Grant.LaunchID != launch || !selected.DispatchReserved {
			t.Error("relay opened without the committed one-shot launch grant")
		}
		return inboxResponseStream(t, request, relay.InboxDispatchArmed), nil
	}
	d.srv.managed.hub = hub

	r, body, _ := d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/start", d.csrf(d.alice), map[string]any{"definition": "deliver", "commit": commit, "goal": "Fresh exact second pass"})
	expectStatus(t, "start second pass", r, body, http.StatusOK)
	d.srv.steps.wait()
	_, native := d.calls()
	step := d.last()
	if step.Sequence != 2 || step.AttestedCommit != commit || step.State != "delivering" || len(native) != 0 || len(operations) != 2 || operations[0] != relay.InboxAdmit || operations[1] != relay.InboxDispatch {
		t.Fatalf("signed second-pass dispatcher did not hold one committed inbox path: %+v, operations=%v native=%d", step, operations, len(native))
	}
}
