package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Владелец ограждает эфемерный root RPC на реальном SSH-open (#292).
// SPEC: Задержанный адаптер не открывает корневой канал после уже завершённого
// вмешательства владельца; payload ребёнка не раскрывается.
// IMPLEMENTATION: Owner-effect gate проверяется на реальном SSH-open.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralAttestationCannotOpenAfterOwnerIntervention(t *testing.T) {
	d, intent, allocation := ephemeralAttestationServerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	actor, err := d.srv.store.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	d.worker = actor
	var effects atomic.Int32
	_, _, messages := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		effects.Add(1)
		return relay.SessionResponse{Outcome: "failed", Text: "unexpected root effect"}
	})
	actual := d.srv.managed.hub.(sessionRelay)
	entered, resume := make(chan struct{}), make(chan struct{})
	d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(ctx context.Context, uid string, generation int64, request relay.SessionRequest) (relay.SessionResponse, error) {
		close(entered)
		select {
		case <-resume:
		case <-ctx.Done():
			return relay.SessionResponse{}, ctx.Err()
		}
		return actual.CallSession(ctx, uid, generation, request)
	}}
	done := make(chan error, 1)
	go func() {
		_, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-open")
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("attestation did not reach delayed relay boundary")
	}
	sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := d.srv.store.AcquireOwnerActorOperation(ctx, actor, "owner-input", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := d.srv.store.FinishActorOperation(ctx, owner, true); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("owner-fenced attestation returned success")
		}
	case <-ctx.Done():
		t.Fatal("fenced attestation did not finish")
	}
	if effects.Load() != 0 || messages.Load() != 0 {
		t.Fatalf("effect opened after owner intervention: effects=%d messages=%d", effects.Load(), messages.Load())
	}
	got, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
	if err != nil || got.State == "attested" || got.AttestedCommit != "" {
		t.Fatalf("interrupted call retained proof: %+v %v", got, err)
	}
	lease, err := d.srv.store.CurrentActorOperation(ctx, allocation.ActorUID)
	if err != nil || lease.State != "unconfirmed" || lease.InterruptedBy == "" {
		t.Fatalf("owner intervention lost old hold: %+v %v", lease, err)
	}
}
