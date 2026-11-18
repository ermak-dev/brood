package deliverylog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var testScope = Scope{ActorUID: "actor-uid", Incarnation: 7}

func openTest(t *testing.T, dir string, capacity int) *Journal {
	t.Helper()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := Open(context.Background(), dir, testScope, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestConfirmedDeliveryReopensAsDuplicateWithoutAnotherEffect(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 4)
	first, err := j.Begin(ctx, "step:one", []byte("exact message"))
	if err != nil || first.State != New {
		t.Fatalf("first admission: %+v %v", first, err)
	}
	if err := j.Finish(ctx, first.Ticket, Delivered); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTest(t, dir, 4)
	duplicate, err := reopened.Begin(ctx, "step:one", []byte("exact message"))
	if err != nil || duplicate.State != Duplicate {
		t.Fatalf("lost confirmed receipt after restart: %+v %v", duplicate, err)
	}
	if _, err := reopened.Begin(ctx, "step:one", []byte("changed message")); !errors.Is(err, ErrConflict) {
		t.Fatalf("ID reused for changed payload: %v", err)
	}
}

func TestReservedOrUnknownDeliveryNeverBecomesRetryPermission(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(map[bool]string{false: "crash-before-settlement", true: "unknown"}[settled], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			j := openTest(t, dir, 2)
			first, err := j.Begin(ctx, "answer:one", []byte("reply"))
			if err != nil {
				t.Fatal(err)
			}
			if settled {
				if err := j.Finish(ctx, first.Ticket, Unconfirmed); err != nil {
					t.Fatal(err)
				}
			}
			_ = j.Close()
			reopened := openTest(t, dir, 2)
			for range 3 {
				retry, err := reopened.Begin(ctx, "answer:one", []byte("reply"))
				if err != nil || retry.State != Unknown {
					t.Fatalf("unknown delivery became resend or receipt: %+v %v", retry, err)
				}
			}
		})
	}
}

func TestKnownNotSentCanRetryButOldTicketCannotSettleNewAttempt(t *testing.T) {
	ctx := context.Background()
	j := openTest(t, t.TempDir(), 2)
	first, err := j.Begin(ctx, "step", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, first.Ticket, NotSent); err != nil {
		t.Fatal(err)
	}
	second, err := j.Begin(ctx, "step", []byte("body"))
	if err != nil || second.State != New || second.Ticket.Attempt != 2 {
		t.Fatalf("known not-sent retry: %+v %v", second, err)
	}
	if err := j.Finish(ctx, first.Ticket, Delivered); !errors.Is(err, ErrConflict) {
		t.Fatalf("late result settled another attempt: %v", err)
	}
	if err := j.Finish(ctx, second.Ticket, Delivered); err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, second.Ticket, Delivered); err != nil {
		t.Fatalf("exact settle retry: %v", err)
	}
	if err := j.Finish(ctx, second.Ticket, NotSent); !errors.Is(err, ErrConflict) {
		t.Fatalf("settlement rewrote confirmed delivery: %v", err)
	}
}

func TestFullJournalRetainsOldIDsAndRefusesNewEffects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 1)
	first, err := j.Begin(ctx, "old", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, first.Ticket, Delivered); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := j.Begin(ctx, "new", []byte("other")); !errors.Is(err, ErrFull) {
			t.Fatalf("full journal forgot history: %v", err)
		}
	}
	duplicate, err := j.Begin(ctx, "old", []byte("body"))
	if err != nil || duplicate.State != Duplicate {
		t.Fatal("full journal lost old receipt")
	}
	_ = j.Close()
	j = openTest(t, dir, 1)
	duplicate, err = j.Begin(ctx, "old", []byte("body"))
	if err != nil || duplicate.State != Duplicate {
		t.Fatal("restart lost full-journal receipt")
	}
}

func TestNotSentRetryBudgetIsBoundedWithoutForgettingID(t *testing.T) {
	ctx := context.Background()
	j := openTest(t, t.TempDir(), 2)
	for range MaxAttempts {
		admission, err := j.Begin(ctx, "retry", []byte("body"))
		if err != nil || admission.State != New {
			t.Fatalf("retry admission: %+v %v", admission, err)
		}
		if err := j.Finish(ctx, admission.Ticket, NotSent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.Begin(ctx, "retry", []byte("body")); !errors.Is(err, ErrAttempts) {
		t.Fatalf("retry budget ignored: %v", err)
	}
	if _, err := j.Begin(ctx, "retry", []byte("changed")); !errors.Is(err, ErrConflict) {
		t.Fatalf("budget erased payload binding: %v", err)
	}
}

func TestConcurrentReopenedJournalsAdmitOnlyOneEffect(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = openTest(t, dir, 16)
	var effects atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := Open(ctx, dir, testScope, 16)
			if err != nil {
				t.Error(err)
				return
			}
			defer j.Close()
			admission, err := j.Begin(ctx, "same", []byte("body"))
			if err != nil {
				t.Error(err)
				return
			}
			if admission.State == New {
				effects.Add(1)
				if err := j.Finish(ctx, admission.Ticket, Delivered); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if effects.Load() != 1 {
		t.Fatalf("admitted effects=%d", effects.Load())
	}
}

func TestJournalScopeAndCorruptionAreNotSilentlyReinitialized(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 2)
	if _, err := j.Begin(ctx, "pending", []byte("body")); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	if _, err := Open(ctx, dir, Scope{ActorUID: "actor-uid", Incarnation: 8}, 2); !errors.Is(err, ErrScope) {
		t.Fatalf("new incarnation inherited old journal: %v", err)
	}
	path := filepath.Join(dir, "delivery.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"version":1`)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := Open(ctx, dir, testScope, 2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("partial record was discarded: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatal("corruption recovery erased journal")
	}
}

func TestJournalRejectsSymlinksAndSharedPermissions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "delivery.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir, testScope, 2); err == nil {
		t.Fatal("journal followed actor-controlled symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "untouched" {
		t.Fatal("symlink changed target")
	}
	shared := t.TempDir()
	if err := os.Chmod(shared, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, shared, testScope, 2); err == nil {
		t.Fatal("group-writable journal directory accepted")
	}
}

func TestCancelledOpenDoesNotPoisonAnUncreatedJournal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, dir, testScope, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "delivery.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled open left a poisoned journal: %v", err)
	}
}

func TestJournalRefusesRelativeDirectoryAndChangedFilePermissions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}
	if j, err := Open(ctx, relative, testScope, 2); err == nil {
		_ = j.Close()
		t.Fatal("relative state directory accepted")
	}
	j := openTest(t, dir, 2)
	if err := os.Chmod(filepath.Join(dir, "delivery.jsonl"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Begin(ctx, "not-private", []byte("body")); err == nil {
		t.Fatal("admission ignored changed journal permissions")
	}
}

func TestJournalRefusesHardLinksAndSymlinkDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 2)
	_ = j.Close()
	if err := os.Link(filepath.Join(dir, "delivery.jsonl"), filepath.Join(dir, "other-name")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir, testScope, 2); err == nil {
		t.Fatal("journal with extra hard link accepted")
	}
	alias := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, alias, testScope, 2); err == nil {
		t.Fatal("state directory symlink accepted")
	}
}

func TestUnknownOutcomeCannotBeUpgradedByLateSettlement(t *testing.T) {
	ctx := context.Background()
	j := openTest(t, t.TempDir(), 2)
	admission, err := j.Begin(ctx, "unknown", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, admission.Ticket, Unconfirmed); err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, admission.Ticket, Delivered); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown outcome upgraded without proof: %v", err)
	}
	foreign := admission.Ticket
	foreign.Scope.Incarnation++
	if err := j.Finish(ctx, foreign, Unconfirmed); !errors.Is(err, ErrScope) {
		t.Fatalf("foreign ticket reused: %v", err)
	}
}

func TestJournalStoresFingerprintNotMessageAndRejectsAmbiguousRecords(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 2)
	payload := []byte("private-message-SENTINEL-never-store-plaintext")
	admission, err := j.Begin(ctx, "confirmed", payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, admission.Ticket, Delivered); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	path := filepath.Join(dir, "delivery.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, payload) {
		t.Fatal("journal retained plaintext message")
	}
	ambiguous := bytes.Replace(data, []byte(`"state":"delivered"`), []byte(`"state":"unconfirmed","state":"delivered"`), 1)
	if bytes.Equal(data, ambiguous) {
		t.Fatal("corruption probe did not change a record")
	}
	if err := os.WriteFile(path, ambiguous, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir, testScope, 2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("duplicate field became confirmed receipt: %v", err)
	}
}

func TestJournalRejectsCaseAliasedOutcomeFields(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 2)
	admission, err := j.Begin(ctx, "uncertain", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, admission.Ticket, Unconfirmed); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	path := filepath.Join(dir, "delivery.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	aliased := bytes.Replace(data, []byte(`"state":"unconfirmed"`), []byte(`"STATE":"unconfirmed","state":"delivered"`), 1)
	if bytes.Equal(data, aliased) {
		t.Fatal("case-alias probe did not change journal")
	}
	if err := os.WriteFile(path, aliased, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir, testScope, 2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("case-aliased outcome became confirmed proof: %v", err)
	}
}

func TestJournalLocalLockWaitRespectsCancellation(t *testing.T) {
	j := openTest(t, t.TempDir(), 1)
	j.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := j.Begin(ctx, "cancelled-local", []byte("body")); done <- err }()
	select {
	case err := <-done:
		j.mu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("local lock cancellation: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		j.mu.Unlock()
		<-done
		t.Fatal("same-handle lock ignored caller cancellation")
	}
}

func TestJournalFileLockWaitRespectsCancellation(t *testing.T) {
	dir := t.TempDir()
	j := openTest(t, dir, 1)
	file, err := os.OpenFile(filepath.Join(dir, "delivery.jsonl"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := j.Begin(ctx, "blocked", []byte("body")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("file lock ignored deadline: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("file lock wait exceeded cancellation budget")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	admission, err := j.Begin(context.Background(), "after-lock", []byte("body"))
	if err != nil || admission.State != New {
		t.Fatalf("lock cancellation created a delivery: %+v %v", admission, err)
	}
}

func TestJournalIdentitySurvivesReopenAndChangesForNewStorage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 2)
	identity := j.Identifier()
	if len(identity) != 64 {
		t.Fatalf("missing durable journal identity: %q", identity)
	}
	_ = j.Close()
	if reopened := openTest(t, dir, 2); reopened.Identifier() != identity {
		t.Fatal("reopen changed journal identity")
	}
	fresh := openTest(t, t.TempDir(), 2)
	if fresh.Identifier() == identity {
		t.Fatal("new storage reused old journal identity")
	}
	admission, err := fresh.Begin(ctx, "ticket", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := admission.Ticket
	foreign.JournalID = identity
	if err := fresh.Finish(ctx, foreign, Delivered); !errors.Is(err, ErrScope) {
		t.Fatalf("ticket from another journal settled current state: %v", err)
	}
}

func TestJournalReadOnlyLookupNeverAdmitsMissingOrKnownNotSent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j := openTest(t, dir, 1)
	before, err := os.ReadFile(filepath.Join(dir, "delivery.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := j.Lookup(ctx, "missing", []byte("body"))
	if err != nil || missing != Absent {
		t.Fatalf("missing lookup became permission: %s %v", missing, err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "delivery.jsonl"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only lookup changed journal")
	}
	first, err := j.Begin(ctx, "missing", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if state, err := j.Lookup(ctx, "missing", []byte("body")); err != nil || state != Unknown {
		t.Fatalf("pending lookup: %s %v", state, err)
	}
	if err := j.Finish(ctx, first.Ticket, NotSent); err != nil {
		t.Fatal(err)
	}
	if state, err := j.Lookup(ctx, "missing", []byte("body")); err != nil || state != NotSentRecorded {
		t.Fatalf("known not-sent lookup: %s %v", state, err)
	}
	if _, err := j.Lookup(ctx, "missing", []byte("changed")); !errors.Is(err, ErrConflict) {
		t.Fatalf("lookup ignored payload binding: %v", err)
	}
}

func TestLegacyJournalDoesNotInventAnIdentityOrEraseHistory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "delivery.jsonl")
	legacy := []byte("{\"version\":1,\"actorUid\":\"actor-uid\",\"incarnation\":7}\n")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	j := openTest(t, dir, 2)
	if j.Identifier() != "" {
		t.Fatal("legacy state received an invented identity")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, legacy) {
		t.Fatal("legacy open rewrote history")
	}
}

func TestCancelledAdmissionDoesNotCreateDeliveryRecord(t *testing.T) {
	j := openTest(t, t.TempDir(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := j.Begin(ctx, "cancelled", []byte("body")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission: %v", err)
	}
	live, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if admission, err := j.Begin(live, "next", []byte("body")); err != nil || admission.State != New {
		t.Fatalf("cancelled call consumed capacity: %+v %v", admission, err)
	}
}
