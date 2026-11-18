package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// BUG: Отмена фонового admission не прерывает ожидание чужого project gate.
// HYPOTHESIS: Group adapter вызывает admission без context; остановка loop
// ждёт gate даже после отмены, хотя новый запрос ещё не получил permission.
// SOLUTION: Group adapter передаёт свой context в cancellable project admission.
// RESULT: FIXED — direct admission и actual service stop выходят до release чужого
// gate без provider reads/effects; исходный вызов воспроизвёл оба зависания.
// STATUS: FIXED
type observedAdmissionStore struct {
	app.Store
	armed   atomic.Bool
	once    sync.Once
	entered chan struct{}
}

func (s *observedAdmissionStore) SwarmByID(id string) (app.Swarm, bool, error) {
	value, found, err := s.Store.SwarmByID(id)
	if s.armed.Load() {
		s.once.Do(func() { close(s.entered) })
	}
	return value, found, err
}

func TestEphemeralGroupAdmissionCancellationLeavesHeldProjectGate(t *testing.T) {
	for _, mode := range []string{"admission", "service-stop"} {
		t.Run(mode, func(t *testing.T) { checkEphemeralGateCancellation(t, mode == "service-stop") })
	}
}

func checkEphemeralGateCancellation(t *testing.T, service bool) {
	t.Helper()
	d, intents, provider := groupCapacityFixture(t, true)
	observed := &observedAdmissionStore{Store: d.srv.gateway, entered: make(chan struct{})}
	d.srv.app = app.NewService(observed, d.srv.provision, time.Now, store.NewID)
	hold := ephemeralGateHold{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hold.release) }) }
	defer release()
	held := make(chan error, 1)
	go func() { held <- d.srv.app.AdmitWorkload(d.master.DelegatorID, d.swarm, hold) }()
	<-hold.entered
	observed.armed.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var stop func()
	if service {
		stop = startEphemeralRecoveryForTest(t, d, ctx)
		defer stop()
	} else {
		go func() {
			_, _, err := d.srv.allocateQueuedEphemeralGroup(ctx, intents[0].ID, nil, 4)
			done <- err
		}()
	}
	<-observed.entered
	cancel()
	if service {
		go func() {
			stop()
			done <- ctx.Err()
		}()
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled group admission returned %v", err)
		}
	case <-time.After(time.Second):
		release()
		<-held
		<-done
		t.Fatal("cancelled group admission still waited for the occupied project gate")
	}
	if len(provider.checked) != 0 || providerEffectsOf(provider.managedFakePVE).creates != 0 {
		t.Fatal("cancelled gate wait reached provider or physical admission")
	}
	release()
	<-held
}
