//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"brood.dev/go/brood/proxmox/egress"
	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

func TestSelectedOfflinePolicyAndDurableDeliveryShareSupervisorLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, runtime := newFake(), newFake()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	var journal *deliverylog.Journal
	selectedStarted := false
	d := dependencies{
		euid:    func() int { return 0 },
		network: func(context.Context) (service, error) { return network, nil },
		deliveryJournal: func(ctx context.Context, directory string, scope deliverylog.Scope) (*deliverylog.Journal, error) {
			if directory != "/private/delivery" || selectedStarted {
				t.Error("journal lost explicit setting or initialized after actor")
			}
			var err error
			journal, err = deliverylog.Open(ctx, path, scope, 2)
			return journal, err
		},
		start: func(context.Context, []string) (actorRuntime, error) {
			t.Error("selected offline policy fell back to legacy starter")
			return runtime, nil
		},
		startPolicy: func(_ context.Context, policy *egress.Policy) (actorRuntime, error) {
			selectedStarted = true
			if journal == nil || policy == nil || policy.AllowsDNS("tool.example") {
				t.Error("durable delivery changed selected offline policy")
			}
			return runtime, nil
		},
		relay: func(_ context.Context, cfg relay.ClientConfig) error {
			if !selectedStarted || journal == nil || !cfg.MessageDeduplication || cfg.MessageJournalID != journal.Identifier() {
				t.Error("selected runtime lost durable capability")
			}
			cancel()
			return context.Canceled
		},
	}
	env := append(selectedEnv(t, `{"version":1,"rules":[]}`), "BROOD_GUEST_DELIVERY_STATE_DIR=/private/delivery")
	if err := dispatch(ctx, nil, func() []string { return env }, d); err != nil {
		t.Fatal(err)
	}
	if !selectedStarted || !runtime.closed || !network.closed || journal == nil {
		t.Fatal("integrated supervisor did not complete lifecycle")
	}
	if _, err := journal.Begin(context.Background(), "closed", []byte("work")); !errors.Is(err, deliverylog.ErrClosed) {
		t.Fatal("integrated supervisor leaked journal")
	}
}
