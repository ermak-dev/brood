//go:build linux

package guest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

func durableJournal(t *testing.T, dir string, capacity int) *deliverylog.Journal {
	t.Helper()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := deliverylog.Open(context.Background(), dir, deliverylog.Scope{ActorUID: "actor", Incarnation: 1}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestDurableMessageConfirmedReplayDoesNotWriteSessionAgain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := durableJournal(t, dir, 2)
	req := relay.MessageRequest{To: "worker", From: "swarm:one", Body: "Step 1", JournalID: j.Identifier(), DeliveryID: "step:one", WaitSec: 0}
	var calls atomic.Int32
	transport := func(_ context.Context, got relay.MessageRequest) relay.MessageResponse {
		calls.Add(1)
		if got.DeliveryID != "" || got.JournalID != "" || got.Replay || got.Body != req.Body || got.From != req.From || got.To != req.To {
			t.Fatal("helper received changed routing/body or journal-only metadata")
		}
		observer := durableJournal(t, dir, 2)
		payload, _ := json.Marshal(req)
		admission, err := observer.Begin(ctx, req.DeliveryID, payload)
		if err != nil || admission.State != deliverylog.Unknown {
			t.Fatal("session write began before durable reservation")
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: transport}
	first := handler.receive(ctx, req)
	if !first.OK || first.Duplicate || first.NotSent {
		t.Fatalf("first delivery: %+v", first)
	}
	_ = j.Close()
	handler.journal = durableJournal(t, dir, 2)
	duplicate := handler.receive(ctx, req)
	if !duplicate.OK || !duplicate.Duplicate || duplicate.NotSent || calls.Load() != 1 {
		t.Fatalf("lost server reply repeated model write: %+v calls=%d", duplicate, calls.Load())
	}
	req.Body = "Different task"
	conflict := handler.receive(ctx, req)
	if conflict.OK || conflict.Duplicate || conflict.NotSent || calls.Load() != 1 {
		t.Fatalf("changed payload reused receipt: %+v", conflict)
	}
}

func TestDurableMessageRefusesOldIdentityAfterJournalReplacement(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	dir := filepath.Join(parent, "current")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal := durableJournal(t, dir, 2)
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", JournalID: journal.Identifier(), DeliveryID: "previously-confirmed"}
	calls := 0
	handler := durableMessageHandler{journal: journal, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	if response := handler.receive(ctx, req); !response.OK {
		t.Fatal(response)
	}
	_ = journal.Close()
	if err := os.Rename(dir, filepath.Join(parent, "retained-old-journal")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	handler.journal = durableJournal(t, dir, 2)
	response := handler.receive(ctx, req)
	if response.OK || response.Duplicate || response.NotSent || calls != 1 {
		t.Fatalf("replacement journal repeated old delivery: %+v model writes=%d", response, calls)
	}
}

func TestDurableMessageReplayNeverAdmitsMissingIDAfterValidPrefixRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := durableJournal(t, dir, 2)
	path := filepath.Join(dir, "delivery.jsonl")
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", DeliveryID: "lost-entry", JournalID: j.Identifier()}
	calls := 0
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	if response := handler.receive(ctx, req); !response.OK {
		t.Fatal(response)
	}
	_ = j.Close()
	if err := os.WriteFile(path, header, 0600); err != nil {
		t.Fatal(err)
	}
	handler.journal = durableJournal(t, dir, 2)
	if handler.journal.Identifier() != req.JournalID {
		t.Fatal("rollback probe accidentally changed journal identity")
	}
	req.Replay = true
	response := handler.receive(ctx, req)
	if response.OK || response.NotSent || response.Duplicate || calls != 1 {
		t.Fatalf("missing replay became a new model write: %+v calls=%d", response, calls)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(header) {
		t.Fatal("read-only replay reserved a new delivery")
	}
}

func TestDurableMessageReplayCannotUseRolledBackNotSentFromEarlierAttempt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := durableJournal(t, dir, 2)
	path := filepath.Join(dir, "delivery.jsonl")
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", DeliveryID: "retry", JournalID: j.Identifier()}
	calls := 0
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		if calls == 1 {
			return messageNotSent("not ready")
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	if response := handler.receive(ctx, req); !response.NotSent {
		t.Fatal(response)
	}
	previous, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if response := handler.receive(ctx, req); !response.OK {
		t.Fatal(response)
	}
	_ = j.Close()
	if err := os.WriteFile(path, previous, 0600); err != nil {
		t.Fatal(err)
	}
	handler.journal = durableJournal(t, dir, 2)
	req.Replay = true
	response := handler.receive(ctx, req)
	if response.OK || response.NotSent || response.Duplicate || calls != 2 {
		t.Fatalf("old not-sent receipt cleared uncertainty of later write: %+v calls=%d", response, calls)
	}
}

func TestDurableMessageReplayReturnsOnlyRetainedProofWithoutTransport(t *testing.T) {
	for _, outcome := range []string{"delivered", "not-sent"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			j := durableJournal(t, t.TempDir(), 2)
			req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", DeliveryID: "retained", JournalID: j.Identifier()}
			calls := 0
			handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
				calls++
				if outcome == "not-sent" {
					return relay.MessageResponse{NotSent: true, Verdict: "not ready"}
				}
				return relay.MessageResponse{OK: true, Verdict: "delivered"}
			}}
			first := handler.receive(ctx, req)
			if first.OK != (outcome == "delivered") || first.NotSent != (outcome == "not-sent") {
				t.Fatal("initial test outcome missing")
			}
			req.Replay = true
			response := handler.receive(ctx, req)
			if calls != 1 {
				t.Fatal("read-only replay invoked session helper")
			}
			if outcome == "delivered" && (!response.OK || !response.Duplicate || response.NotSent) {
				t.Fatalf("confirmed replay proof lost: %+v", response)
			}
			if outcome == "not-sent" && (response.OK || response.Duplicate || response.NotSent) {
				t.Fatalf("historical not-sent receipt authorized another attempt: %+v", response)
			}
		})
	}
}

func TestDurableMessageUnknownAndPendingNeverDispatchAgain(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed-helper", true: "abandoned-reservation"}[pending], func(t *testing.T) {
			ctx := context.Background()
			j := durableJournal(t, t.TempDir(), 2)
			req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", JournalID: j.Identifier(), DeliveryID: "unknown"}
			calls := 0
			handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
				calls++
				return relay.MessageResponse{Verdict: "submitted; delivery unconfirmed"}
			}}
			if pending {
				payload, _ := json.Marshal(req)
				if _, err := j.Begin(ctx, req.DeliveryID, payload); err != nil {
					t.Fatal(err)
				}
			}
			first := handler.receive(ctx, req)
			req.Replay = true
			second := handler.receive(ctx, req)
			want := 1
			if pending {
				want = 0
			}
			if first.OK || second.OK || first.Duplicate || second.Duplicate || first.NotSent || second.NotSent || calls != want {
				t.Fatalf("unknown became receipt or retry: %+v %+v calls=%d", first, second, calls)
			}
		})
	}
}

func TestDurableMessageNotSentCanRetryButContradictionsCannotConfirm(t *testing.T) {
	ctx := context.Background()
	j := durableJournal(t, t.TempDir(), 3)
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", JournalID: j.Identifier(), DeliveryID: "not-sent"}
	calls := 0
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		if calls == 1 {
			return relay.MessageResponse{NotSent: true, Verdict: "session is not ready"}
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	if first := handler.receive(ctx, req); first.OK || !first.NotSent {
		t.Fatalf("known refusal lost proof: %+v", first)
	}
	if second := handler.receive(ctx, req); !second.OK || second.Duplicate || calls != 2 {
		t.Fatalf("known not-sent did not retry: %+v", second)
	}
	req.DeliveryID = "contradictory"
	handler.transport = func(context.Context, relay.MessageRequest) relay.MessageResponse {
		return relay.MessageResponse{OK: true, NotSent: true, Verdict: "invalid"}
	}
	if bad := handler.receive(ctx, req); bad.OK || bad.NotSent || bad.Duplicate {
		t.Fatalf("contradictory helper evidence accepted: %+v", bad)
	}
	if retry := handler.receive(ctx, req); retry.OK || retry.Duplicate || retry.NotSent {
		t.Fatalf("contradiction became known receipt: %+v", retry)
	}
}

func TestDurableMessageCannotAcknowledgeAfterReceiptPersistenceFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := durableJournal(t, dir, 2)
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", JournalID: j.Identifier(), DeliveryID: "lost-persistence"}
	calls := 0
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		if err := os.Chmod(filepath.Join(dir, "delivery.jsonl"), 0644); err != nil {
			t.Fatal(err)
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	response := handler.receive(ctx, req)
	if response.OK || response.NotSent || response.Duplicate || calls != 1 {
		t.Fatalf("unpersisted receipt reported confirmed: %+v", response)
	}
	if err := os.Chmod(filepath.Join(dir, "delivery.jsonl"), 0600); err != nil {
		t.Fatal(err)
	}
	retry := handler.receive(ctx, req)
	if retry.OK || retry.Duplicate || retry.NotSent || calls != 1 {
		t.Fatalf("lost settlement repeated session write: %+v calls=%d", retry, calls)
	}
}

func TestDurableMessageBindsRoutingAndWaitWithoutInventingNewIdle(t *testing.T) {
	ctx := context.Background()
	j := durableJournal(t, t.TempDir(), 2)
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", JournalID: j.Identifier(), DeliveryID: "with-wait", WaitSec: 1}
	calls := 0
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		return relay.MessageResponse{OK: true, Verdict: "delivered; idle confirmed"}
	}}
	if response := handler.receive(ctx, req); !response.OK {
		t.Fatal(response)
	}
	if response := handler.receive(ctx, req); !response.OK || !response.Duplicate || !strings.Contains(response.Verdict, "idle status not rechecked") || strings.Contains(response.Verdict, "idle confirmed") {
		t.Fatalf("duplicate invented current idle: %+v", response)
	}
	for _, field := range []string{"to", "from", "wait"} {
		changed := req
		switch field {
		case "to":
			changed.To = "other"
		case "from":
			changed.From = "another"
		case "wait":
			changed.WaitSec = 0
		}
		if response := handler.receive(ctx, changed); response.OK || response.Duplicate || response.NotSent {
			t.Fatalf("changed %s reused confirmed receipt: %+v", field, response)
		}
	}
	if calls != 1 {
		t.Fatalf("metadata replay caused %d model writes", calls)
	}
}

func TestDurableMessageSettlesObservedReceiptAfterChannelCancellation(t *testing.T) {
	j := durableJournal(t, t.TempDir(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", JournalID: j.Identifier(), DeliveryID: "cancelled-response"}
	calls := 0
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return true }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		cancel()
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	if response := handler.receive(ctx, req); !response.OK || ctx.Err() == nil {
		t.Fatalf("observed receipt was lost on cancellation: %+v", response)
	}
	if response := handler.receive(context.Background(), req); !response.OK || !response.Duplicate || calls != 1 {
		t.Fatalf("cancelled reply repeated confirmed model write: %+v", response)
	}
}

func TestDurableMessageChecksSupervisorBeforeReservingAndLeavesLegacyUnjournaled(t *testing.T) {
	ctx := context.Background()
	j := durableJournal(t, t.TempDir(), 1)
	req := relay.MessageRequest{To: "worker", From: "owner", Body: "work", JournalID: j.Identifier(), DeliveryID: "protected"}
	calls := 0
	handler := durableMessageHandler{journal: j, supervisor: func() bool { return false }, transport: func(context.Context, relay.MessageRequest) relay.MessageResponse {
		calls++
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	if response := handler.receive(ctx, req); response.OK || response.NotSent || calls != 0 {
		t.Fatal("non-supervisor admitted durable write")
	}
	handler.supervisor = func() bool { return true }
	legacy := req
	legacy.DeliveryID = ""
	legacy.JournalID = ""
	if response := handler.receive(ctx, legacy); !response.OK || response.Duplicate || calls != 1 {
		t.Fatalf("legacy send changed: %+v", response)
	}
	if response := handler.receive(ctx, req); !response.OK || response.Duplicate || calls != 2 {
		t.Fatalf("non-supervisor or legacy call consumed journal capacity: %+v", response)
	}
}
