//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

func TestDeliveryJournalConfigIsExplicitAndPreservedAcrossBootstrap(t *testing.T) {
	env := validEnv(t)
	legacy, err := parseConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.deliveryDirectory != "" || legacy.relay.MessageDeduplication || legacy.relay.MessageJournalID != "" {
		t.Fatal("legacy config advertised durable delivery")
	}
	configured, err := parseConfig(append(env, "BROOD_GUEST_DELIVERY_STATE_DIR=/private/delivery"))
	if err != nil {
		t.Fatal(err)
	}
	if configured.deliveryDirectory != "/private/delivery" {
		t.Fatal("explicit state directory lost")
	}
	if configured.relay.MessageDeduplication {
		t.Fatal("configuration advertised capability before opening journal")
	}
	if !strings.Contains(strings.Join(runtimeEnv(configured), "\n"), "BROOD_GUEST_DELIVERY_STATE_DIR=/private/delivery\n") {
		t.Fatal("bootstrap lost explicit delivery state directory")
	}
	for _, path := range []string{"", "relative", "/private/../delivery", "/private/delivery/"} {
		if _, err := parseConfig(append(env, "BROOD_GUEST_DELIVERY_STATE_DIR="+path)); err == nil {
			t.Errorf("invalid explicit directory accepted: %q", path)
		}
	}
}

func runtimeDeliveryConfig(t *testing.T, directory string) config {
	t.Helper()
	env := append(validEnv(t), "BROOD_GUEST_STAGE=runtime")
	if directory != "" {
		env = append(env, "BROOD_GUEST_DELIVERY_STATE_DIR="+directory)
	}
	cfg, err := parseConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSupervisorAdvertisesDeliveryOnlyAfterJournalAndClosesItOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := newFake()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	var journal *deliverylog.Journal
	started := false
	d := dependencies{deliveryJournal: func(ctx context.Context, got string, scope deliverylog.Scope) (*deliverylog.Journal, error) {
		if got != "/private/delivery" || scope.ActorUID != "actor-1" || scope.Incarnation != 1 || started {
			t.Error("journal initialization escaped bootstrap scope/order")
		}
		var err error
		journal, err = deliverylog.Open(ctx, path, scope, 2)
		return journal, err
	},
		start: func(context.Context) (actorRuntime, error) { started = true; return runtime, nil },
		relay: func(ctx context.Context, cfg relay.ClientConfig) error {
			if journal == nil || !cfg.MessageDeduplication || cfg.MessageJournalID != journal.Identifier() || cfg.HandleMessage == nil {
				t.Error("relay started without retained journal and handler")
			}
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if err := supervise(ctx, runtimeDeliveryConfig(t, "/private/delivery"), d); err != nil {
		t.Fatal(err)
	}
	if journal == nil {
		t.Fatal("journal was never initialized")
	}
	if _, err := journal.Begin(context.Background(), "closed", []byte("work")); !errors.Is(err, deliverylog.ErrClosed) {
		t.Fatal("journal survived supervisor shutdown")
	}
	if !runtime.closed {
		t.Fatal("runtime resources leaked")
	}
}

func TestSupervisorDeliveryInitializationFailsBeforeActorOrRelay(t *testing.T) {
	for _, failure := range []string{"unavailable", "nil", "legacy", "missing-opener"} {
		t.Run(failure, func(t *testing.T) {
			called := false
			d := dependencies{start: func(context.Context) (actorRuntime, error) { called = true; return newFake(), nil }, relay: func(context.Context, relay.ClientConfig) error { called = true; return nil }}
			if failure != "missing-opener" {
				d.deliveryJournal = func(ctx context.Context, _ string, scope deliverylog.Scope) (*deliverylog.Journal, error) {
					if failure == "unavailable" {
						return nil, errors.New("private-token")
					}
					if failure == "nil" {
						return nil, nil
					}
					path := t.TempDir()
					if err := os.Chmod(path, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "delivery.jsonl"), []byte("{\"version\":1,\"actorUid\":\"actor-1\",\"incarnation\":1}\n"), 0600); err != nil {
						t.Fatal(err)
					}
					return deliverylog.Open(ctx, path, scope, 2)
				}
			}
			err := supervise(context.Background(), runtimeDeliveryConfig(t, "/private/delivery"), d)
			if err == nil || strings.Contains(err.Error(), "private-token") || called {
				t.Fatalf("unsafe delivery startup failure: %v downstream=%v", err, called)
			}
		})
	}
}

func TestSupervisorWithoutExplicitDeliveryDirectoryKeepsLegacyRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := newFake()
	d := dependencies{deliveryJournal: func(context.Context, string, deliverylog.Scope) (*deliverylog.Journal, error) {
		t.Error("legacy boot opened journal")
		return nil, nil
	}, start: func(context.Context) (actorRuntime, error) { return runtime, nil }, relay: func(ctx context.Context, cfg relay.ClientConfig) error {
		if cfg.MessageDeduplication || cfg.MessageJournalID != "" {
			t.Error("legacy boot advertised durable capability")
		}
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}}
	if err := supervise(ctx, runtimeDeliveryConfig(t, ""), d); err != nil {
		t.Fatal(err)
	}
}
