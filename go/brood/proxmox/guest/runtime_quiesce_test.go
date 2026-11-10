//go:build linux

package guest

// FEATURE: Root-proven quiet window for workspace attestation (#183).
// SPEC: No actor process exists while the callback runs; the fixed actor is
// relaunched afterwards; an unproven stop keeps enforcement and fails closed.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type failingCloseProcess struct{ fakeProcess }

func (p *failingCloseProcess) Close() error {
	*p.events = append(*p.events, "process-close-failed")
	return errors.New("descendants alive")
}

func TestQuiesceStopsActorRunsAndRelaunches(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	err = h.Quiesce(context.Background(), func(ctx context.Context) error {
		events = append(events, "run")
		return errors.New("checkout refused")
	})
	if err == nil || err.Error() != "checkout refused" {
		t.Fatalf("callback result lost: %v", err)
	}
	if err := h.Quiesce(context.Background(), func(context.Context) error { events = append(events, "run"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"workspace", "rules-install", "services-start", "process-start", "process-close", "run", "process-start", "process-close", "run", "process-start", "process-close", "services-close", "rules-delete"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v", events)
	}
}

func TestQuiesceFailsClosedWhenActorStopIsUnproven(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) {
		events = append(events, "process-start")
		return &failingCloseProcess{fakeProcess{done: make(chan struct{}), events: &events}}, nil
	}
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	err = h.Quiesce(context.Background(), func(context.Context) error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("callback ran without a proven stop: %v", err)
	}
	if err := h.Wait(); err == nil {
		t.Fatal("runtime survived an unproven actor stop")
	}
	if strings.Contains(strings.Join(events, ","), "rules-delete") {
		t.Fatalf("enforcement removed while actor processes may live: %v", events)
	}
}

func TestQuiesceRelaunchFailureStopsRuntime(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	launches := 0
	c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) {
		launches++
		events = append(events, "process-start")
		if launches > 1 {
			return nil, errors.New("exec failed")
		}
		return &fakeProcess{done: make(chan struct{}), events: &events}, nil
	}
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Quiesce(context.Background(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("relaunch failure reported as success")
	}
	if err := h.Wait(); err == nil || !strings.Contains(err.Error(), "relaunch") {
		t.Fatalf("runtime result: %v", err)
	}
	if events[len(events)-1] != "rules-delete" {
		t.Fatalf("no actor process remains, enforcement must be torn down in order: %v", events)
	}
}

func TestQuiesceCallbackCanceledByRuntimeStop(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- h.Quiesce(context.Background(), func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stopped quiesce reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback not canceled by runtime stop")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.Join(events, ","), "process-start") != 1 {
		t.Fatalf("actor relaunched after stop: %v", events)
	}
	if err := h.Quiesce(context.Background(), func(context.Context) error { t.Error("ran after stop"); return nil }); err == nil {
		t.Fatal("quiesce accepted after stop")
	}
}
