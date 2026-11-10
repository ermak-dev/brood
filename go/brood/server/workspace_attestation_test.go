package server

// FEATURE: Server verification of workspace attestations (#183).
// SPEC: The key the server derives from its relay secret matches the token the
// guest received at bootstrap; another incarnation's token never verifies.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/attest"
)

func TestWorkspaceAttestationKeyMatchesGuestBootstrap(t *testing.T) {
	ts, alice, _, id, pve, _ := managedSetup(t)
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "attested"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	ts.srv.managed.reconcileOnce(context.Background())
	env := pve.spec.Env
	generation, err := strconv.ParseInt(env["BROOD_GUEST_GENERATION"], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	registry := attest.NewRegistry(15*time.Minute, 16)
	claim, err := registry.Issue(attest.Request{ActorUID: env["BROOD_GUEST_ACTOR_UID"], Generation: generation, Swarm: id, Step: "request-1", Origin: "https://git.example.com/group/project", Commit: strings.Repeat("ab", 20)})
	if err != nil {
		t.Fatal(err)
	}
	key := workspaceAttestationKey(ts.srv.managed.registry.relaySecret)
	// Гость подписывает ключом из токена, который получил в bootstrap env.
	sign := func(token string) attest.Attestation {
		sig, err := attest.Sign(attest.Key(token), claim)
		if err != nil {
			t.Fatal(err)
		}
		return attest.Attestation{Claim: claim, Signature: sig}
	}
	if _, err := registry.Consume(sign(env["BROOD_GUEST_RELAY_TOKEN"]), key); err != nil {
		t.Fatalf("guest bootstrap token does not verify: %v", err)
	}
	claim, _ = registry.Issue(attest.Request{ActorUID: claim.ActorUID, Generation: generation, Swarm: id, Step: "request-2", Origin: claim.Origin, Commit: claim.Commit})
	next := guestToken(ts.srv.managed.registry.relaySecret, claim.ActorUID, generation+1)
	if _, err := registry.Consume(sign(next), key); !errors.Is(err, attest.ErrSignature) {
		t.Fatalf("token of another incarnation verified: %v", err)
	}
}
