//go:build linux

package guest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

func TestInboxLocalHelperCannotSelectWorkOrReuseAClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	inbox, err := openScopedSupervisorInbox(ctx, base, deliverylog.Scope{ActorUID: "actor-1", Incarnation: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	prompt, err := json.Marshal(relay.MessageRequest{To: "worker", From: "operator", Body: "exact workflow instruction\n"})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "step-1", prompt)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := NewInboxHandoff(ctx, inbox, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	ticket := deliverylog.InboxTicket{Receipt: receipt, LaunchID: "launch-1", PermitID: "permit-1"}
	addr := &net.UnixAddr{Name: filepath.Join(root, "handoff.sock"), Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	call := func(raw string, authorize InboxConsumerAuthenticator) ([]byte, error) {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			c, err := listener.AcceptUnix()
			if err == nil {
				defer c.Close()
				err = ServeInboxHelperConnection(ctx, c, handoff, authorize)
			}
			done <- err
		}()
		client, err := net.DialUnix("unix", nil, addr)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(client, raw); err != nil {
			t.Fatal(err)
		}
		closeErr := client.CloseWrite()
		result, readErr := io.ReadAll(client)
		serverErr := <-done
		// Linux resets a Unix stream closed with unread oversized input. Only
		// accept that transport symptom with the precise rejection and no bytes.
		expectedReject := len(raw) > len(inboxHelperRequest)+1 && errors.Is(serverErr, ErrInboxHandoffUnavailable) && len(result) == 0
		if closeErr != nil && !(expectedReject && errors.Is(closeErr, syscall.ENOTCONN)) {
			t.Fatal(closeErr)
		}
		if readErr != nil && !(expectedReject && errors.Is(readErr, syscall.ECONNRESET)) {
			t.Fatal(readErr)
		}
		return result, serverErr
	}
	authorized := func(_ context.Context, c *net.UnixConn) (string, error) {
		cred, err := inboxHelperCredentials(c)
		if err != nil || cred.Uid != uint32(os.Geteuid()) || cred.Pid != int32(os.Getpid()) {
			return "", errors.New("unexpected loopback peer")
		}
		return "launch-1", nil
	}
	if body, err := call("{\"operation\":\"take\"}\n", nil); err == nil || len(body) != 0 {
		t.Fatalf("unauthenticated client got bytes: %v", err)
	}
	if body, err := call("{\"operation\":\"take\",\"deliveryId\":\"other\"}\n", authorized); err == nil || len(body) != 0 {
		t.Fatalf("actor-selected target was accepted: %v", err)
	}
	if body, err := call("{\"operation\":\"take\"}\nextra\n", authorized); err == nil || len(body) != 0 {
		t.Fatalf("extra request was accepted: %v", err)
	}
	if body, err := call(inboxHelperRequest, authorized); err != nil || string(body) != inboxHelperWaiting {
		t.Fatalf("a live waiter did not get the non-claim response: %v", err)
	}
	if status, err := inbox.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest); err != nil || status.State != deliverylog.InboxQueued {
		t.Fatalf("waiting consumed an offer: %+v %v", status, err)
	}
	if err := handoff.Offer(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	body, err := call(inboxHelperRequest, authorized)
	var offer InboxHelperOffer
	if err != nil || json.Unmarshal(body, &offer) != nil || offer.Text != "exact workflow instruction\n" || offer.DeliveryID != receipt.DeliveryID {
		t.Fatalf("exact work was not released: %v", err)
	}
	if body, err := call("{\"operation\":\"take\"}\n", authorized); err == nil || len(body) != 0 {
		t.Fatalf("same launch got another copy: %v", err)
	}
	status, err := inbox.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest)
	if err != nil || status.State != deliverylog.InboxClaimed {
		t.Fatalf("helper response was not preceded by a durable claim: %+v %v", status, err)
	}
}
