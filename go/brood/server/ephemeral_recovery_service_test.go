package server

import (
	"context"
	"testing"
	"time"
)

// FEATURE: Background recovery соединяет atomic group admission с lifecycle.
// SPEC: Запуск/Close принадлежат серверу; stop ждёт завершения loop, повторный
// запуск читает retained grants и не повторяет Create.
func startEphemeralRecoveryForTest(t *testing.T, d *dispatchServer, ctx context.Context) func() {
	t.Helper()
	return d.srv.startEphemeralChildRecovery(ctx)
}

func TestEphemeralRecoveryStartsAutomaticallyWithServer(t *testing.T) {
	d, intents, provider := groupCapacityFixture(t, true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for providerEffectsOf(provider.managedFakePVE).creates != 2 {
		select {
		case <-ctx.Done():
			history, _ := d.srv.store.GetEphemeralChildHistory(context.Background(), intents[0].ID)
			t.Fatalf("server startup did not activate whole-group recovery: %+v", history)
		case <-ticker.C:
		}
	}
}

func TestEphemeralRecoveryServiceStartsAndStops(t *testing.T) {
	d, _, provider := groupCapacityFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stop := startEphemeralRecoveryForTest(t, d, ctx)
	defer stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for providerEffectsOf(provider.managedFakePVE).creates != 2 {
		select {
		case <-ctx.Done():
			t.Fatal("background loop did not admit and create the complete group")
		case <-ticker.C:
		}
	}
	stop()
	before := providerEffectsOf(provider.managedFakePVE)
	queued, err := d.srv.store.QueuedEphemeralChildIntents(context.Background(), 0)
	if err != nil || len(queued) != 0 {
		t.Fatalf("background loop left a partial allocation: %+v %v", queued, err)
	}
	restartCtx, restartCancel := context.WithCancel(context.Background())
	stopRestart := startEphemeralRecoveryForTest(t, d, restartCtx)
	restartCancel()
	stopRestart()
	if after := providerEffectsOf(provider.managedFakePVE); after != before {
		t.Fatalf("restart replayed a retained physical grant: before=%+v after=%+v", before, after)
	}
}
