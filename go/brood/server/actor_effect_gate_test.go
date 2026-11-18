package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestActorEffectGatesReleaseCancelledScope(t *testing.T) {
	var gates actorEffectGates
	hold, err := gates.acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := gates.acquire(ctx, "a", "b"); !errors.Is(err, context.Canceled) {
		if release != nil {
			release()
		}
		t.Fatalf("cancelled acquisition: %v", err)
	}
	gates.mu.Lock()
	entry, ok := gates.entries["b"]
	if len(gates.entries) != 1 || !ok || entry.users != 1 {
		t.Errorf("cancelled scope leaked entries: %v", gates.entries)
	}
	gates.mu.Unlock()
	hold()
	if len(gates.entries) != 0 {
		t.Fatalf("released gate leaked entries: %v", gates.entries)
	}
}

func TestActorEffectGatesOrderAndDeduplicateScopes(t *testing.T) {
	var gates actorEffectGates
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for n := range 20 {
		wg.Go(func() {
			keys := []string{"a", "b", "a"}
			if n%2 == 0 {
				keys = []string{"b", "a", "b"}
			}
			for range 50 {
				release, err := gates.acquire(ctx, keys...)
				if err != nil {
					t.Errorf("scope acquisition failed: %v", err)
					return
				}
				release()
			}
		})
	}
	wg.Wait()
	if len(gates.entries) != 0 {
		t.Fatalf("completed scopes leaked entries: %v", gates.entries)
	}
}

func TestActorEffectGatesDoNotBlockOtherActors(t *testing.T) {
	var gates actorEffectGates
	hold, err := gates.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := gates.acquire(ctx, "b")
	if err != nil {
		t.Fatalf("unrelated actor blocked: %v", err)
	}
	release()
}
