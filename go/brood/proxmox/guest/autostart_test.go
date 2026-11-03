//go:build linux

package guest

// FEATURE: Root supervisor starts the Claude session at boot (#148).
// SPEC: The fixed UID 1000 helper is retried until the session exists or the
// supervisor stops; every failure is reported with fixed text and never stops
// the supervisor.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAutostartRetriesAndReportsUntilStarted(t *testing.T) {
	var attempts int
	var reports []string
	launch := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("private helper output")
		}
		return nil
	}
	autostartSession(context.Background(), launch, func(err error) { reports = append(reports, err.Error()) }, func(int) time.Duration { return time.Millisecond })
	if attempts != 3 || len(reports) != 2 {
		t.Fatalf("attempts %d reports %q", attempts, reports)
	}
	for _, report := range reports {
		if strings.Contains(report, "private") || !strings.Contains(report, "retry") {
			t.Fatalf("unsafe or unclear report: %q", report)
		}
	}
}

func TestAutostartStopsWithSupervisor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		autostartSession(ctx, func(context.Context) error { return errors.New("failed") }, func(error) { cancel() }, func(int) time.Duration { return time.Hour })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("autostart outlived the supervisor")
	}
}

func TestAutostartBackoffIsBounded(t *testing.T) {
	if autostartDelay(0) != time.Second || autostartDelay(40) != 30*time.Second {
		t.Fatalf("delays %v %v", autostartDelay(0), autostartDelay(40))
	}
}

func TestAutostartHelperSpecIsFixed(t *testing.T) {
	spec := autostartHelperSpec()
	if spec.Executable != "/proc/self/exe" || !reflect.DeepEqual(spec.Args, []string{"--brood-guest-autostart"}) || spec.UID != 1000 || spec.GID != 1000 || spec.Dir != "/workspace/home" || !reflect.DeepEqual(spec.Env, actorEnv()) {
		t.Fatalf("%+v", spec)
	}
}
