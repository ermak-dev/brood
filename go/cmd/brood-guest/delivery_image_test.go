//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

func TestImageDeliveryConfigurationIsReadOnlyAfterArtifactReexec(t *testing.T) {
	previous := artifactRoot
	artifactRoot = newRoot(t)
	t.Cleanup(func() { artifactRoot = previous })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	d := supervisorDeps(t, &events, cancel)
	d.deliveryDirectory = func(context.Context) (string, error) {
		t.Error("bootstrap read image runtime configuration before upgrading")
		return "", errors.New("must not be called")
	}
	set := newReleaseSet("delivery-image")
	d.artifacts = func(ctx context.Context, _ relay.ClientConfig, release guest.ArtifactRelease) error {
		events = append(events, "artifacts")
		server := &fakeServer{set: set}
		return release(ctx, set.manifest(), server.download)
	}
	d.reexec = func(_ string, env []string) error {
		events = append(events, "reexec")
		for _, entry := range env {
			if strings.HasPrefix(entry, "BROOD_GUEST_DELIVERY_") {
				t.Error("image setting contaminated closed legacy bootstrap grammar")
			}
		}
		runtimeConfig, err := parseConfig(env)
		if err != nil {
			return err
		}
		runtimeDeps := supervisorDeps(t, &events, cancel)
		runtimeDeps.deliveryDirectory = func(context.Context) (string, error) {
			events = append(events, "image-config")
			return "", nil
		}
		return supervise(ctx, runtimeConfig, runtimeDeps)
	}
	if err := dispatch(ctx, nil, func() []string { return validEnv(t) }, d); err != nil {
		t.Fatal(err)
	}
	if want := []string{"artifacts", "reexec", "image-config", "services", "relay"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("configuration crossed bootstrap/runtime boundary: %v", events)
	}
}

func TestRuntimeImageDirectoryOpensJournalBeforeSecretsAndAdvertisesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	d := supervisorDeps(t, &events, cancel)
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	var opened *deliverylog.Journal
	d.deliveryDirectory = func(context.Context) (string, error) {
		events = append(events, "image-config")
		return "/var/lib/brood/delivery", nil
	}
	d.deliveryJournal = func(ctx context.Context, directory string, scope deliverylog.Scope) (*deliverylog.Journal, error) {
		events = append(events, "journal")
		if directory != "/var/lib/brood/delivery" || scope.ActorUID != "actor-1" || scope.Incarnation != 1 {
			t.Error("image delivery configuration lost runtime scope")
		}
		var err error
		opened, err = deliverylog.Open(ctx, path, scope, 2)
		return opened, err
	}
	d.secrets = func(context.Context, relay.ClientConfig) error {
		events = append(events, "secrets")
		return nil
	}
	d.relay = func(ctx context.Context, cfg relay.ClientConfig) error {
		events = append(events, "relay")
		if opened == nil || !cfg.MessageDeduplication || cfg.MessageJournalID != opened.Identifier() {
			t.Error("image-enabled relay lacks retained delivery identity")
		}
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	if err := supervise(ctx, runtimeDeliveryConfig(t, ""), d); err != nil {
		t.Fatal(err)
	}
	if want := []string{"image-config", "journal", "secrets", "services", "relay"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("image delivery initialization order: %v", events)
	}
	if opened == nil {
		t.Fatal("image did not opt in to delivery journal")
	}
	if _, err := opened.Begin(context.Background(), "closed", []byte("work")); !errors.Is(err, deliverylog.ErrClosed) {
		t.Fatal("image-enabled journal survived shutdown")
	}
}

func TestInvalidImageConfigurationFailsBeforeSecretsActorAndRelay(t *testing.T) {
	var events []string
	d := supervisorDeps(t, &events, nil)
	d.deliveryDirectory = func(context.Context) (string, error) {
		return "", errors.New("private configuration canary")
	}
	d.secrets = func(context.Context, relay.ClientConfig) error {
		t.Error("unsafe image configuration reached secrets")
		return nil
	}
	err := supervise(context.Background(), runtimeDeliveryConfig(t, ""), d)
	if err == nil || strings.Contains(err.Error(), "canary") || len(events) != 0 {
		t.Fatalf("unsafe image configuration continued or leaked details: %v, %v", err, events)
	}
}

func TestExplicitBootstrapDeliveryDirectoryOverridesImageConfiguration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	d := supervisorDeps(t, &events, cancel)
	d.deliveryDirectory = func(context.Context) (string, error) {
		t.Error("explicit root bootstrap setting was overridden by image configuration")
		return "", errors.New("must not be called")
	}
	d.deliveryJournal = func(_ context.Context, directory string, _ deliverylog.Scope) (*deliverylog.Journal, error) {
		if directory != "/explicit/delivery" {
			t.Error("explicit delivery directory lost")
		}
		return nil, errors.New("stop before services")
	}
	if err := supervise(ctx, runtimeDeliveryConfig(t, "/explicit/delivery"), d); err == nil {
		t.Fatal("failed explicit journal silently fell back to image or legacy")
	}
	if len(events) != 0 {
		t.Fatalf("failed explicit journal reached services: %v", events)
	}
}
