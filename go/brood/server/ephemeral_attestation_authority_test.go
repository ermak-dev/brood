package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Локальная authority перепроверяется после ожидания и ответа root RPC.
// SPEC: Отзыв прав не создаёт разрешение или proof; удержанный эффект не повторяется.
// IMPLEMENTATION: Права проверяются до резерва и при фиксации proof.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralAttestationRechecksAuthorityAfterConnectionWait(t *testing.T) {
	d, intent, allocation := ephemeralAttestationServerFixture(t)
	ctx := context.Background()
	before, messages := d.calls()
	base := d.srv.managed.hub.(connectHub).commandSessionHub
	var hookErr error
	d.srv.managed.hub = connectHub{commandSessionHub: base, connected: func(commandSessionHub, string, int64) bool {
		hookErr = d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true)
		return true
	}}
	if _, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1"); err == nil {
		t.Fatal("revoked owner obtained effect permission")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if _, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revocation wrote partial attempt: %v", err)
	}
	if _, err := d.srv.store.CurrentActorOperation(ctx, allocation.ActorUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revocation wrote workspace hold: %v", err)
	}
	after, sent := d.calls()
	if len(before) != len(after) || len(messages) != len(sent) {
		t.Fatal("revoked owner reached guest")
	}
}

func TestEphemeralAttestationRejectsProofAfterOwnerOrParentChanges(t *testing.T) {
	for _, scenario := range []string{"owner", "role", "intervention"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, allocation := ephemeralAttestationServerFixture(t)
			ctx := context.Background()
			before, messages := d.calls()
			original := d.reply
			d.reply = func(call guestAttest) (relay.SessionResponse, error) {
				response, err := original(call)
				if err != nil {
					return response, err
				}
				switch scenario {
				case "owner":
					err = d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true)
				case "role":
					err = d.srv.store.BindSwarmRole(ctx, store.SwarmRoleBinding{SwarmID: d.swarm, Role: "build", ActorUID: d.worker.UID})
				case "intervention":
					actor, readErr := d.srv.store.GetManagedActor(ctx, allocation.ActorUID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					sw, readErr := d.srv.store.GetSwarm(ctx, d.swarm)
					if readErr != nil {
						t.Fatal(readErr)
					}
					_, err = d.srv.store.AcquireOwnerActorOperation(ctx, actor, "owner-input", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				}
				if err != nil {
					t.Fatal(err)
				}
				return response, nil
			}
			if _, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1"); err == nil {
				t.Fatal("proof after revocation was accepted")
			}
			got, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
			if err != nil || got.State == "attested" || got.AttestedCommit != "" {
				t.Fatalf("revoked proof retained: %+v %v", got, err)
			}
			if _, err := d.srv.store.CurrentActorOperation(ctx, allocation.ActorUID); err != nil {
				t.Fatalf("revocation released unknown hold: %v", err)
			}
			_, _ = d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1")
			after, sent := d.calls()
			if len(after) != len(before)+1 || len(sent) != len(messages) {
				t.Fatal("revocation repeated effect or delivered child payload")
			}
		})
	}
}
