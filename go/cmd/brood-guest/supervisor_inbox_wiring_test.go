//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Publish the inbox capability only from a prepared owned runtime.
func TestSupervisorInboxOptInStartsOnlyAfterPreparation(t *testing.T) {
	for _, ready := range []bool{false, true} {
		name := "absent_protected_loader"
		if ready {
			name = "prepared_loader"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			child := newFake()
			path := t.TempDir()
			if err := os.Chmod(path, 0700); err != nil {
				t.Fatal(err)
			}
			c := runtimeDeliveryConfig(t, "/private/delivery")
			c.supervisorInbox, c.stepLaunchBinding = true, true
			var prepared, started, stopped, relayed bool
			var owner *guest.Runtime
			const receiver = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			d := dependencies{
				deliveryJournal: func(ctx context.Context, _ string, scope deliverylog.Scope) (*deliverylog.Journal, error) {
					return deliverylog.Open(ctx, path, scope, 2)
				},
				prepareInbox: func(_ context.Context, directory string) (preparedInboxStart, error) {
					if started || directory != "/private/delivery" {
						t.Fatal("loader validation was not a pre-actor effect with scoped durable state")
					}
					prepared = true
					if !ready {
						return nil, errors.New("private-loader-test-sentinel")
					}
					return func(_ context.Context, runtime *guest.Runtime, cfg *relay.ClientConfig) (io.Closer, error) {
						if runtime != owner || !started || cfg.ActorUID != "actor-1" || cfg.Generation != 1 || cfg.InboxReceiverID != "" {
							t.Fatal("broker was not started with the exact owning runtime and inactive capability")
						}
						cfg.InboxReceiverID = receiver
						cfg.HandleInbox = func(context.Context, []byte) ([]byte, error) { return nil, nil }
						return inboxFixtureCloser(func() error { stopped = true; return nil }), nil
					}, nil
				},
				start: func(ctx context.Context) (actorRuntime, error) {
					if !ready || !prepared {
						t.Fatal("actor started before valid protected loader state")
					}
					started = true
					var err error
					owner, err = guest.Start(ctx, guest.Config{Dependencies: &guest.Dependencies{
						EUID: func() int { return 0 }, Workspace: func() error { return nil },
						Launch: func(context.Context, guest.ActorSpec) (guest.Process, error) { return child, nil },
					}})
					return owner, err
				},
				relay: func(ctx context.Context, got relay.ClientConfig) error {
					relayed = true
					if got.InboxReceiverID != receiver || got.HandleInbox == nil || got.SessionFactory == nil || !got.MessageDeduplication {
						t.Error("relay did not receive exact inbox and ordinary session configuration")
					}
					cancel()
					return ctx.Err()
				},
			}
			t.Setenv("PATH", "/usr/bin:/bin")
			err := supervise(ctx, c, d)
			if !ready {
				if err == nil || !prepared || started || stopped || relayed || strings.Contains(err.Error(), "private-loader-test-sentinel") {
					t.Fatalf("unprepared loader did not fail closed with safe diagnostics: %v", err)
				}
				return
			}
			if err != nil || !prepared || !started || !relayed || !stopped || !child.closed {
				t.Fatalf("prepared startup did not own and close the exact relay endpoint: %v", err)
			}
		})
	}
}

type inboxFixtureCloser func() error

func (c inboxFixtureCloser) Close() error { return c() }
