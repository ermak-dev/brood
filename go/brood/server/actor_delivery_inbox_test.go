package server

// FEATURE: Explicit supervisor inbox custody for a retained actor delivery (#291).
// SPEC: Commit the exact selection before admission; after a lost response, use
// only read-only status. Recheck current authority, release effect gates before
// reading the response, and do not publish a native outcome or dispatch permit.
// IMPLEMENTATION: The explicit coordinator commits selection and custody
// separately, reusing source-specific authority and actor effect gates.
// STATUS: Statically reviewed; execution deferred by request.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// Use a structural check so the test can report the missing coordinator before
// implementation, even without the relay adapters under development.
func inboxDeliveryCall(t *testing.T, d *actorDeliveryService) func(context.Context, string, string) (relay.InboxDisposition, error) {
	t.Helper()
	coordinator, ok := any(d).(interface {
		deliverInbox(context.Context, string, string) (relay.InboxDisposition, error)
	})
	if !ok {
		t.Fatal("missing explicit inbox delivery coordinator")
	}
	return coordinator.deliverInbox
}

type inboxDeliveryRelay struct {
	*durableAnswerRelay
	receiver string
	open     func(context.Context, relay.InboxRequest) (net.Conn, error)
}

func (h *inboxDeliveryRelay) InboxReceiverID(uid string, incarnation int64) string {
	if uid != h.uid || incarnation != h.incarnation {
		return ""
	}
	return h.receiver
}

func (h *inboxDeliveryRelay) DialInbox(ctx context.Context, request relay.InboxRequest) (net.Conn, error) {
	return h.open(ctx, request)
}

func installInboxDeliveryRelay(t *testing.T, s *Server, hub *durableAnswerRelay) *inboxDeliveryRelay {
	t.Helper()
	inbox := &inboxDeliveryRelay{durableAnswerRelay: hub, receiver: strings.Repeat("b", 64)}
	s.managed.hub = inbox
	s.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		t.Error("selected inbox delivery fell back to native transport")
		return relay.MessageResponse{}, errors.New("unexpected native transport")
	}
	return inbox
}

func inboxChildFixture(t *testing.T) (*dispatchServer, store.ActorDeliveryIntent, *inboxDeliveryRelay) {
	t.Helper()
	d, job, _, hub, _ := childDeliveryFixture(t)
	d.srv.steps.close()
	intent, err := d.srv.prepareEphemeralChildDelivery(context.Background(), d.master.DelegatorID, job.Parent, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, intent, installInboxDeliveryRelay(t, d.srv, hub)
}

func inboxResponseStream(t *testing.T, request relay.InboxRequest, disposition relay.InboxDisposition) net.Conn {
	t.Helper()
	frame, err := relay.EncodeInboxResponse(relay.InboxResponse{Target: request.Target, Disposition: disposition})
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := net.Pipe()
	go func() {
		defer writer.Close()
		_, _ = writer.Write(frame)
	}()
	return reader
}

func TestInboxDeliveryLostReplyRecoversByExactReadOnlyLookup(t *testing.T) {
	ctx := context.Background()
	d, intent, hub := inboxChildFixture(t)
	call := inboxDeliveryCall(t, d.srv.actorDelivery)
	if _, err := call(ctx, "", d.master.DelegatorID); err == nil {
		t.Fatal("empty delivery ID accepted")
	}
	sum := sha256.Sum256([]byte(intent.Input.Payload))
	target := relay.InboxTarget{ActorUID: intent.Input.ActorUID, Incarnation: intent.Input.Incarnation, ReceiverID: hub.receiver, DeliveryID: intent.ID, Digest: hex.EncodeToString(sum[:])}
	var requests []relay.InboxRequest
	var admitted bool
	hub.open = func(_ context.Context, request relay.InboxRequest) (net.Conn, error) {
		requests = append(requests, request)
		// Read independently of the dial context so an uncommitted selection
		// cannot satisfy this check through a transaction carried by that context.
		lookup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		selected, err := d.srv.store.ActorDeliveryInboxStatus(lookup, intent.ID)
		if err != nil || selected.Target.ReceiverID != target.ReceiverID || selected.Target.Digest != target.Digest || selected.Custody || selected.DispatchReserved || request.Target != target {
			t.Fatalf("channel opened without the exact committed selection: %+v %+v %v", request.Target, selected, err)
		}
		switch request.Operation {
		case relay.InboxAdmit:
			if admitted || string(request.Payload) != intent.Input.Payload {
				t.Fatal("repeated admission or changed retained payload")
			}
			admitted = true
			reader, writer := net.Pipe()
			writer.Close() // Durable guest custody, but the first reply never arrives.
			return reader, nil
		case relay.InboxStatus:
			if !admitted || len(request.Payload) != 0 {
				t.Fatal("recovery carried a payload or ran before admission")
			}
			return inboxResponseStream(t, request, relay.InboxCustodyRecorded), nil
		default:
			t.Fatalf("unexpected operation: %q", request.Operation)
			return nil, nil
		}
	}
	if got, err := call(ctx, intent.ID, d.master.DelegatorID); err == nil || got == relay.InboxCustodyRecorded {
		t.Fatalf("lost reply declared custody: %q %v", got, err)
	}
	hub.receiver = strings.Repeat("c", 64)
	if _, err := call(ctx, intent.ID, d.master.DelegatorID); !errors.Is(err, relay.ErrInboxReceiverChanged) || len(requests) != 1 {
		t.Fatalf("recovery used a replacement receiver: requests=%d err=%v", len(requests), err)
	}
	hub.receiver = target.ReceiverID
	d.srv.actorDelivery = newActorDeliveryService(d.srv, nil)
	call = inboxDeliveryCall(t, d.srv.actorDelivery)
	for range 2 {
		if got, err := call(ctx, intent.ID, d.master.DelegatorID); err != nil || got != relay.InboxCustodyRecorded {
			t.Fatalf("exact custody lookup: %q %v", got, err)
		}
	}
	if len(requests) != 2 || requests[0].Operation != relay.InboxAdmit || requests[1].Operation != relay.InboxStatus {
		t.Fatalf("unexpected channel sequence: %+v", requests)
	}
	selected, err := d.srv.store.ActorDeliveryInboxStatus(ctx, intent.ID)
	if err != nil || !selected.Custody || selected.DispatchReserved {
		t.Fatalf("custody not kept separate from dispatch: %+v %v", selected, err)
	}
	history, err := d.srv.store.GetActorDeliveryHistory(ctx, intent.ID)
	if err != nil || len(history) != 0 {
		t.Fatalf("custody became native delivery evidence: %+v %v", history, err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, intent.Input.SourceID)
	if err != nil || work.Task.State != "running" || work.Task.Outcome != "" || work.Task.Report != "" {
		t.Fatalf("custody changed the child outcome: %+v %v", work.Task, err)
	}
}

func TestInboxDispatchLostReplyNeverIssuesAnotherGrant(t *testing.T) {
	ctx := context.Background()
	d, intent, hub := inboxChildFixture(t)
	owner := d.master.DelegatorID
	calls := 0
	hub.open = func(_ context.Context, request relay.InboxRequest) (net.Conn, error) {
		calls++
		if calls == 1 && request.Operation == relay.InboxAdmit {
			return inboxResponseStream(t, request, relay.InboxCustodyRecorded), nil
		}
		if request.Operation != relay.InboxDispatch || request.Grant == nil || request.Grant.LaunchID != "trusted-launch" || len(request.Payload) != 0 {
			t.Fatalf("unexpected permit submission: %v", request.Operation)
		}
		stored, err := d.srv.store.ActorDeliveryInboxStatus(ctx, intent.ID)
		if err != nil || !stored.DispatchReserved {
			t.Fatalf("dispatch opened before durable permit: %+v %v", stored, err)
		}
		return nil, errors.New("reply lost after possible arming")
	}
	if result, err := d.srv.actorDelivery.deliverInbox(ctx, intent.ID, owner); err != nil || result != relay.InboxCustodyRecorded {
		t.Fatalf("custody did not settle: %v", err)
	}
	deny := func(context.Context, store.ActorDeliveryInboxTarget, string) bool { return false }
	if _, err := d.srv.actorDelivery.dispatchInbox(ctx, intent.ID, owner, "trusted-launch", deny); !errors.Is(err, store.ErrActorDeliveryDenied) || calls != 1 {
		t.Fatalf("dispatch opened without trusted launch proof: %v", err)
	}
	allow := func(_ context.Context, target store.ActorDeliveryInboxTarget, launch string) bool {
		return target.DeliveryID == intent.ID && target.ActorUID == intent.Input.ActorUID && launch == "trusted-launch"
	}
	if result, err := d.srv.actorDelivery.dispatchInbox(ctx, intent.ID, owner, "trusted-launch", allow); err == nil || result != "" || calls != 2 {
		t.Fatalf("lost arm response was treated as success: %q %v (calls %d)", result, err, calls)
	}
	if _, err := d.srv.actorDelivery.dispatchInbox(ctx, intent.ID, owner, "trusted-launch", allow); !errors.Is(err, store.ErrActorDeliveryConflict) || calls != 2 {
		t.Fatalf("possible arming issued a second grant: %v", err)
	}
}

func TestInboxDeliveryRechecksRevokedAuthority(t *testing.T) {
	ctx := context.Background()
	d, intent, hub := inboxChildFixture(t)
	call := inboxDeliveryCall(t, d.srv.actorDelivery)
	userID := d.master.DelegatorID
	d.fake.mu.Lock()
	d.fake.beforeNamespaceGet = func() {
		if err := d.srv.store.SetUserDisabled(ctx, userID, true); err != nil {
			t.Error(err)
		}
	}
	d.fake.mu.Unlock()
	hub.open = func(context.Context, relay.InboxRequest) (net.Conn, error) {
		t.Error("inbox channel opened after approver revocation")
		return nil, errors.New("unexpected inbox channel")
	}
	if _, err := call(ctx, intent.ID, userID); err == nil {
		t.Fatal("revoked delivery was allowed")
	}
	if _, err := d.srv.store.ActorDeliveryInboxStatus(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked delivery left a selection: %v", err)
	}
}

func TestInboxDeliveryReleasesBothEffectGatesBeforeWaiting(t *testing.T) {
	d, intent, hub := inboxChildFixture(t)
	call := inboxDeliveryCall(t, d.srv.actorDelivery)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opened := make(chan relay.InboxRequest, 1)
	reader, writer := net.Pipe()
	defer reader.Close()
	defer writer.Close()
	if err := writer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	hub.open = func(ctx context.Context, request relay.InboxRequest) (net.Conn, error) {
		gates := &d.srv.managed.effects
		gates.mu.Lock()
		for _, uid := range []string{d.master.UID, intent.Input.ActorUID} {
			if entry := gates.entries[uid]; entry == nil || len(entry.ready) != 0 {
				t.Errorf("inbox channel opened without holding the %s effect gate", uid)
			}
		}
		gates.mu.Unlock()
		opened <- request
		return reader, nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := call(ctx, intent.ID, d.master.DelegatorID)
		finished <- err
	}()
	var request relay.InboxRequest
	select {
	case request = <-opened:
	case err := <-finished:
		t.Fatalf("channel did not open: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out opening inbox channel")
	}
	release, err := d.srv.managed.effects.acquire(ctx, d.master.UID, intent.Input.ActorUID)
	if err != nil {
		t.Fatalf("effect gates held while waiting for custody: %v", err)
	}
	err = d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true)
	release()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := relay.EncodeInboxResponse(relay.InboxResponse{Target: request.Target, Disposition: relay.InboxCustodyRecorded})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(frame); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case err := <-finished:
		if !errors.Is(err, store.ErrActorDeliveryDenied) {
			t.Fatalf("custody recorded after authority was revoked: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out collecting inbox response")
	}
	selected, err := d.srv.store.ActorDeliveryInboxStatus(ctx, intent.ID)
	if err != nil || selected.Custody || selected.DispatchReserved {
		t.Fatalf("late receipt changed retained evidence: %+v %v", selected, err)
	}
}

func TestInboxDeliveryAbsentStatusDoesNotReadmitOrChangeAnswer(t *testing.T) {
	ctx := context.Background()
	rs, legacy, _ := durableAnswerServer(t)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	swarmID := rs.master.SwarmID
	user, err := rs.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	authorize := func(ctx context.Context) bool {
		return rs.srv.store.CheckActorDeliveryManager(ctx, swarmID, user.ID) == nil
	}
	answer, err := rs.srv.store.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: swarmID, RequestID: id, UserID: user.ID, Response: "Use sqlite."}, authorize)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(relay.MessageRequest{To: rs.worker.Name, From: "operator:alice", Body: operatorMessageBody(requestAnswerMessage(answer, user.Username))})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := rs.srv.store.PrepareActorDelivery(ctx, store.ActorDeliveryInput{SwarmID: swarmID, SourceKind: "answer", SourceID: id, ActorUID: rs.worker.UID, Incarnation: rs.worker.Incarnation, AtespaceUID: rs.worker.AtespaceUID, JournalID: legacy.journal, Payload: string(payload)}, authorize)
	if err != nil {
		t.Fatal(err)
	}
	hub := installInboxDeliveryRelay(t, rs.srv, legacy)
	call := inboxDeliveryCall(t, rs.srv.actorDelivery)
	opens := 0
	hub.open = func(_ context.Context, req relay.InboxRequest) (net.Conn, error) {
		opens++
		if opens == 1 && req.Operation == relay.InboxAdmit {
			return nil, errors.New("channel failed after selection")
		}
		if req.Operation != relay.InboxStatus || len(req.Payload) != 0 {
			t.Fatal("absence allowed a new admission")
		}
		return inboxResponseStream(t, req, relay.InboxReceiptAbsent), nil
	}
	if _, err := call(ctx, intent.ID, user.ID); err == nil {
		t.Fatal("missing channel reported success")
	}
	for range 2 {
		if got, err := call(ctx, intent.ID, user.ID); err != nil || got != relay.InboxReceiptAbsent {
			t.Fatalf("expected a read-only absent observation: %q %v", got, err)
		}
	}
	// Existing manual/background answer delivery is not an inbox opt-in and
	// must not turn a protected selection into a native delivery outcome.
	rs.srv.deliverSwarmAnswer(ctx, user, answer, false)
	got, err := rs.srv.store.GetSwarmRequest(ctx, swarmID, id)
	if err != nil || got.State != answer.State || got.Delivery != "pending" || opens != 3 {
		t.Fatalf("inbox operation changed the answer: %+v opens=%d err=%v", got, opens, err)
	}
	selection, err := rs.srv.store.ActorDeliveryInboxStatus(ctx, intent.ID)
	if err != nil || selection.Custody || selection.DispatchReserved {
		t.Fatalf("absent receipt was mistaken for evidence: %+v %v", selection, err)
	}
}
