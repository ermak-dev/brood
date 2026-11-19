//go:build linux

package deliverylog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// FEATURE: A separate supervisor inbox confirms durable custody, not a Claude turn.
// SPEC: Recover a receipt by exact identity after reconnect, never replay a claim.
func TestInboxReceiptSurvivesReopenAndConflictingPayload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	inbox := openTestInbox(t, dir, 2)
	payload := []byte("instruction A")
	first, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", payload)
	if err != nil || first.InboxID != inbox.Identifier() || first.DeliveryID != "visit-1" || first.Digest != inboxDigest(payload) {
		t.Fatalf("invalid admission receipt: %+v, %v", first, err)
	}
	if err := inbox.Close(); err != nil {
		t.Fatal(err)
	}
	reader := openTestInbox(t, dir, 2)
	status, err := reader.Lookup(ctx, first.InboxID, first.DeliveryID, first.Digest)
	if err != nil || status.Receipt != first || status.State != InboxQueued {
		t.Fatalf("receipt did not survive reopen: %+v, %v", status, err)
	}
	repeated, err := reader.Enqueue(ctx, first.InboxID, "visit-1", payload)
	if err != nil || repeated != first {
		t.Fatalf("exact repeat returned a different receipt: %+v, %v", repeated, err)
	}
	if _, err = reader.Enqueue(ctx, first.InboxID, "visit-1", []byte("instruction B")); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting payload was accepted: %v", err)
	}
	if _, err = reader.Lookup(ctx, first.InboxID, "missing", first.Digest); !errors.Is(err, ErrInboxMissing) {
		t.Fatalf("missing record invented a receipt: %v", err)
	}
	if _, err = reader.Enqueue(ctx, "different-inbox", "visit-1", payload); !errors.Is(err, ErrScope) {
		t.Fatalf("wrong receiver identity was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inbox changed the native receipt journal: %v", err)
	}
}

func TestInboxSingleClaimAcrossHandlesAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := openTestInbox(t, dir, 1)
	second := openTestInbox(t, dir, 1)
	input := []byte("complete retained canonical body")
	receipt, err := first.Enqueue(ctx, first.Identifier(), "visit-1", input)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		ticket  InboxTicket
		payload []byte
		err     error
	}
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan result, 2)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, actor := range []*Inbox{first, second} {
		go func() {
			ready <- struct{}{}
			<-start
			ticket, payload, err := actor.Claim(ctx, receipt, "launch-1", "permit-1")
			results <- result{ticket, payload, err}
		}()
	}
	<-ready
	<-ready
	close(start)
	var ticket InboxTicket
	var admitted int
	for range 2 {
		got := <-results
		if got.err == nil {
			if string(got.payload) != string(input) {
				t.Error("claim changed retained payload")
			}
			admitted++
			ticket = got.ticket
		} else if !errors.Is(got.err, ErrInboxClaimed) || got.payload != nil {
			t.Errorf("duplicate claim returned an unexpected result: %v", got.err)
		}
	}
	if admitted != 1 {
		t.Fatalf("claims admitted: %d", admitted)
	}
	reopened := openTestInbox(t, dir, 1)
	if _, payload, err := reopened.Claim(ctx, receipt, "launch-2", "permit-2"); !errors.Is(err, ErrInboxClaimed) || payload != nil {
		t.Fatalf("uncertain previous handoff became retry permission: %v", err)
	}
	wrong := ticket
	wrong.LaunchID = "launch-2"
	if err := reopened.Report(ctx, wrong, InboxSessionAccepted); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong launch settled a claim: %v", err)
	}
	if err := reopened.Report(ctx, ticket, InboxSessionAccepted); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Report(ctx, ticket, InboxFinished); err != nil {
		t.Fatal(err)
	}
	status, err := first.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest)
	if err != nil || status.Receipt != receipt || status.State != InboxFinished {
		t.Fatalf("downstream outcome changed custody receipt: %+v, %v", status, err)
	}
	if _, err := first.Enqueue(ctx, receipt.InboxID, "new-task", input); !errors.Is(err, ErrFull) {
		t.Fatalf("full inbox evicted retained identity: %v", err)
	}
}

func TestInboxSyncFailureCannotProduceAReceipt(t *testing.T) {
	ctx := context.Background()
	inbox := openTestInbox(t, t.TempDir(), 1)
	var calls int
	inbox.dirSync = func() error {
		calls++
		if calls >= 2 {
			return errors.New("simulated sync failure")
		}
		return inbox.dir.Sync()
	}
	payload := []byte("sensitive work instruction")
	receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", payload)
	if !errors.Is(err, ErrUnavailable) || receipt != (InboxReceipt{}) {
		t.Fatalf("failed publication returned a receipt: %+v, %v", receipt, err)
	}
	if status, err := inbox.Lookup(ctx, inbox.Identifier(), "visit-1", inboxDigest(payload)); !errors.Is(err, ErrUnavailable) || status != (InboxStatus{}) {
		t.Fatalf("readable but unsynced custody was trusted: %+v, %v", status, err)
	}
	inbox.dirSync = nil
	if status, err := inbox.Lookup(ctx, inbox.Identifier(), "visit-1", inboxDigest(payload)); err != nil || status.State != InboxQueued {
		t.Fatalf("repairable durable custody was lost after successful sync: %+v, %v", status, err)
	}
}

func TestInboxUnknownIsNotRearmedOrUpgraded(t *testing.T) {
	ctx := context.Background()
	inbox := openTestInbox(t, t.TempDir(), 1)
	receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", []byte("prompt"))
	if err != nil {
		t.Fatal(err)
	}
	ticket, _, err := inbox.Claim(ctx, receipt, "launch-1", "permit-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Report(ctx, ticket, InboxUnknown); err != nil {
		t.Fatal(err)
	}
	if err := inbox.Report(ctx, ticket, InboxSessionAccepted); !errors.Is(err, ErrConflict) {
		t.Fatalf("an unknown handoff was upgraded: %v", err)
	}
	if _, payload, err := inbox.Claim(ctx, receipt, "launch-1", "permit-1"); !errors.Is(err, ErrInboxClaimed) || payload != nil {
		t.Fatalf("an unknown handoff was rearmed: %v", err)
	}
}

func TestInboxReclaimsOnlyUnpublishedPrivateStaging(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	inbox := openTestInbox(t, dir, 1)
	staged := filepath.Join(dir, inboxStagingFile)
	if err := os.WriteFile(staged, []byte("interrupted, never published"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Lookup(ctx, inbox.Identifier(), "visit-1", inboxDigest([]byte("instruction"))); !errors.Is(err, ErrInboxMissing) {
		t.Fatalf("unpublished staging was trusted: %v", err)
	}
	if _, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", []byte("instruction")); err != nil {
		t.Fatalf("private leftover staging could not be reclaimed: %v", err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging remained after publication: %v", err)
	}
}

func TestInboxPublicationKeepsPrivateModeUnderRestrictiveUmask(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	inbox := openTestInbox(t, dir, 1)
	prior := unix.Umask(0200)
	defer unix.Umask(prior)
	receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, inboxFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("acknowledged snapshot is not private and reopenable: %v %v", info, err)
	}
	reopened := openTestInbox(t, dir, 1)
	status, err := reopened.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest)
	if err != nil || status.Receipt != receipt {
		t.Fatalf("published receipt did not survive reopen: %+v %v", status, err)
	}
}

func TestInboxUsesPinnedDirectoryLock(t *testing.T) {
	dir := t.TempDir()
	first := openTestInbox(t, dir, 1)
	second := openTestInbox(t, dir, 1)
	if err := unix.Flock(int(first.dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(first.dir.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if receipt, err := second.Enqueue(ctx, second.Identifier(), "visit-1", []byte("body")); !errors.Is(err, context.DeadlineExceeded) || receipt != (InboxReceipt{}) {
		t.Fatalf("second handle bypassed the directory lock: %+v %v", receipt, err)
	}
}

func TestInboxPermitIsBoundToOneReceipt(t *testing.T) {
	ctx := context.Background()
	inbox := openTestInbox(t, t.TempDir(), 2)
	first, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-1", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := inbox.Enqueue(ctx, inbox.Identifier(), "visit-2", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	launch, permit := strings.Repeat("a", 128), strings.Repeat("b", 128)
	if _, _, err := inbox.Claim(ctx, first, launch, permit); err != nil {
		t.Fatal(err)
	}
	if _, body, err := inbox.Claim(ctx, second, launch, permit); !errors.Is(err, ErrConflict) || body != nil {
		t.Fatalf("reused permit released another payload: %v", err)
	}
	if status, err := inbox.Lookup(ctx, second.InboxID, second.DeliveryID, second.Digest); err != nil || status.State != InboxQueued {
		t.Fatalf("conflicting claim modified second entry: %+v %v", status, err)
	}
}

func TestInboxReservesClaimMetadataBeforeAcknowledging(t *testing.T) {
	ctx := context.Background()
	inbox := openTestInbox(t, t.TempDir(), MaxInboxEntries)
	// Seed a valid near-full snapshot in one write. The admission under test
	// would fit as queued bytes but leave less than the reserved headroom.
	payload := bytes.Repeat([]byte{'x'}, maxInboxPayload)
	err := inbox.withSnapshot(ctx, inbox.Identifier(), func(state inboxSnapshot) error {
		for n := range 47 {
			state.Entries[fmt.Sprintf("visit-%d", n)] = inboxEntry{Payload: payload, Digest: inboxDigest(payload), State: InboxQueued}
		}
		raw, err := json.Marshal(state)
		if err != nil || len(raw) >= maxInboxBytes-inboxClaimHeadroom {
			t.Fatal("invalid near-full test fixture", len(raw), err)
		}
		return inbox.save(ctx, state, maxInboxBytes-inboxClaimHeadroom)
	})
	if err != nil {
		t.Fatal(err)
	}
	last := payload[:120<<10]
	if receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "last", last); !errors.Is(err, ErrFull) || receipt != (InboxReceipt{}) {
		t.Fatalf("admission consumed claim headroom: %+v %v", receipt, err)
	}
	if _, err := inbox.Lookup(ctx, inbox.Identifier(), "last", inboxDigest(last)); !errors.Is(err, ErrInboxMissing) {
		t.Fatalf("refused entry was retained: %v", err)
	}
	if _, _, err := inbox.Claim(ctx, inbox.receipt("visit-0", inboxDigest(payload)), strings.Repeat("x", 128), strings.Repeat("y", 128)); err != nil {
		t.Fatalf("already admitted work could not be claimed: %v", err)
	}
}

func openTestInbox(t *testing.T, path string, capacity int) *Inbox {
	t.Helper()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	inbox, err := OpenInboxDirectory(context.Background(), dir, Scope{ActorUID: "actor-1", Incarnation: 1}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inbox.Close() })
	return inbox
}
