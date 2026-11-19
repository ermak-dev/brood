package server

// FEATURE: Authorized pass exports disclose receipt milestones, not the authority to dispatch.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

func TestPassTraceAPIExportsSupervisorCustodyWithoutDispatchAuthority(t *testing.T) {
	ctx := context.Background()
	d := newDispatchServer(t)
	d.srv.steps.close()
	d.startPending("deliver")
	s := d.srv.store
	a, err := s.ReserveActorStep(ctx, d.swarm, store.NewID(), stepMessageSender(d.swarm))
	if err != nil {
		t.Fatal(err)
	}
	in, err := preparedInstructionDelivery(a, "Build the exact commit.", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	allow := func(context.Context) bool { return true }
	receiver := strings.Repeat("b", 64)
	intent, err := s.RecordActorStepInboxAttestation(ctx, a, dispatchOrigin, dispatchCommit, in, receiver, allow)
	if err != nil {
		t.Fatal(err)
	}
	target := inboxDeliveryTarget(intent, receiver)
	bob := newClient(t)
	d.login(bob, "bob", "bob-password-1")
	user, err := s.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSwarmMember(ctx, d.swarm, user.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	path := "/api/swarms/" + d.swarm + "/passes/1"
	var permit string
	const launchID = "private-trace-launch"
	check := func(milestone string) {
		t.Helper()
		r, body, raw := d.do(bob, "GET", path, "", nil)
		expectStatus(t, "viewer inbox pass trace", r, body, http.StatusOK)
		step := body["steps"].([]any)[0].(map[string]any)
		instruction := step["instruction"].(map[string]any)
		sum := sha256.Sum256([]byte(in.Payload))
		want := map[string]any{"receiverId": receiver, "payloadDigest": hex.EncodeToString(sum[:]), "milestone": milestone}
		if !reflect.DeepEqual(instruction["supervisorInbox"], want) || len(instruction["attempts"].([]any)) != 0 || step["state"] != "delivering" || step["outcome"] != nil || step["reportedResult"] != nil {
			t.Fatalf("pass trace lost inbox evidence or claimed a native/step result: %s", raw)
		}
		if strings.Contains(raw, launchID) || permit != "" && strings.Contains(raw, permit) {
			t.Fatal("export contains private dispatch authority")
		}
		r, download, _ := d.do(bob, "GET", path+"?download=1", "", nil)
		expectStatus(t, "download inbox pass trace", r, download, http.StatusOK)
		if !reflect.DeepEqual(download, body) {
			t.Fatal("download lost or changed retained evidence")
		}
	}
	check("selected")
	if err := s.RecordActorDeliveryInboxCustody(ctx, target, store.ActorDeliveryInboxAdmitted, allow); err != nil {
		t.Fatal(err)
	}
	check("admitted_to_supervisor_inbox")
	permit, err = s.ReserveActorDeliveryInboxDispatch(ctx, target, launchID, func(_ context.Context, got store.ActorDeliveryInboxTarget, launch string) bool {
		return got == target && launch == launchID
	})
	if err != nil || permit == "" {
		t.Fatalf("fixture could not reserve a permit: %v", err)
	}
	check("dispatch_permit_reserved")
	if err := s.RemoveSwarmMember(ctx, d.swarm, user.ID); err != nil {
		t.Fatal(err)
	}
	r, body, _ := d.do(bob, "GET", path+"?download=1", "", nil)
	expectStatus(t, "revoked inbox export", r, body, http.StatusNotFound)
	if attestations, messages := d.calls(); len(attestations) != 0 || len(messages) != 0 {
		t.Fatal("reading export performed an actor operation")
	}
}
