//go:build linux

package guest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
)

func TestSupervisorDeliveryJournalUsesSeparateRetainedIncarnations(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	scope := deliverylog.Scope{ActorUID: "actor", Incarnation: 1}
	first, err := openScopedDeliveryJournal(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	id := first.Identifier()
	ticket, err := first.Begin(ctx, "retained", []byte("work"))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(ctx, ticket.Ticket, deliverylog.Delivered); err != nil {
		t.Fatal(err)
	}
	first.Close()
	reopened, err := openScopedDeliveryJournal(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Identifier() != id {
		t.Fatal("boot changed journal identity")
	}
	if result, err := reopened.Lookup(ctx, "retained", []byte("work")); err != nil || result != deliverylog.Duplicate {
		t.Fatal("boot lost confirmed receipt")
	}
	scope.Incarnation++
	next, err := openScopedDeliveryJournal(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Identifier() == id {
		t.Fatal("new incarnation reused previous journal identity")
	}
	if result, err := next.Lookup(ctx, "retained", []byte("work")); err != nil || result != deliverylog.Absent {
		t.Fatal("new incarnation inherited old receipt")
	}
}

func TestSupervisorInboxIsSeparateFromNativeJournalAndIncarnation(t *testing.T) {
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
	scope := deliverylog.Scope{ActorUID: "actor", Incarnation: 1}
	legacy, err := openScopedDeliveryJournal(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	first, err := openScopedSupervisorInbox(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	body := []byte("canonical instruction")
	receipt, err := first.Enqueue(ctx, first.Identifier(), "step-1", body)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Identifier() == receipt.InboxID {
		t.Fatal("inbox reused legacy journal identity")
	}
	if got, err := legacy.Lookup(ctx, "step-1", body); err != nil || got != deliverylog.Absent {
		t.Fatalf("inbox custody leaked into native receipt: %s, %v", got, err)
	}
	reopened, err := openScopedSupervisorInbox(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if status, err := reopened.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest); err != nil || status.Receipt != receipt {
		t.Fatalf("reopened supervisor lost custody: %+v, %v", status, err)
	}
	scope.Incarnation++
	next, err := openScopedSupervisorInbox(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Identifier() == receipt.InboxID {
		t.Fatal("new incarnation inherited old inbox identity")
	}
}

func TestSupervisorDeliveryJournalRejectsSymlinkAndSharedScope(t *testing.T) {
	for _, kind := range []string{"symlink", "shared"} {
		t.Run(kind, func(t *testing.T) {
			path := t.TempDir()
			if err := os.Chmod(path, 0700); err != nil {
				t.Fatal(err)
			}
			base, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			child := filepath.Join(path, "actor")
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), child); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(child, 0770); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(child, 0770); err != nil {
					t.Fatal(err)
				}
			}
			if j, err := openScopedDeliveryJournal(context.Background(), base, deliverylog.Scope{ActorUID: "actor", Incarnation: 1}); err == nil {
				j.Close()
				t.Fatal("unsafe scope directory accepted")
			}
		})
	}
}

func TestSupervisorDeliveryDirectoryRequiresExactPrivatePermissions(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0500); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if deliveryDirectoryOwned(dir, uint32(os.Geteuid()), true) {
		t.Fatal("read-only directory accepted as 0700 delivery state")
	}
}

func TestSupervisorDeliveryJournalRefusesWritableAncestor(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "private")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if dir, err := openDeliveryRoot(path); err == nil {
		dir.Close()
		t.Fatal("actor-writable temporary ancestor accepted for root history")
	}
}
