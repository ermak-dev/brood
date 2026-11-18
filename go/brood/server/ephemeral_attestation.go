package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type ephemeralChildAttestor struct {
	claims *attest.Registry
}

func newEphemeralChildAttestor() *ephemeralChildAttestor {
	return &ephemeralChildAttestor{claims: attest.NewRegistry(stepAttestationTTL, stepAttestationLimit)}
}

// История не разрешает повторный RPC. Эфемерные claims не делят registry
// с графовыми шагами; ключ child берётся из глобального insertion cursor.
func (s *Server) attestEphemeralChild(ctx context.Context, userID string, parent store.SwarmChildParent, intentID, attemptID string) (store.EphemeralChildAttestation, error) {
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return store.EphemeralChildAttestation{}, err
	}
	if intent.Parent != parent || intent.QueuePosition < 1 || s.managed == nil {
		return store.EphemeralChildAttestation{}, store.ErrSwarmChildDenied
	}
	authorize := func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if a.Action != "attest_ephemeral" || a.Parent != parent {
			return store.ErrSwarmChildDenied
		}
		if err := s.authorizeSwarmOwnerIntent(ctx, userID, parent.Principal.SwarmID); err != nil {
			return err
		}
		return s.managed.childAuthority(ctx, a)
	}
	if _, err := s.store.GetEphemeralChildAttestation(ctx, intentID); err == nil {
		return s.store.ReserveEphemeralChildAttestation(ctx, intentID, attemptID, authorize)
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.EphemeralChildAttestation{}, err
	}
	hub, ok := s.managed.hub.(sessionRelay)
	if !ok || s.ephemeralAttestor == nil || s.managed.registry == nil || len(s.managed.registry.relaySecret) < 32 {
		return store.EphemeralChildAttestation{}, errors.New("ephemeral workspace attestation is unavailable")
	}
	allocation, err := s.store.GetEphemeralChildAllocation(ctx, intentID)
	if err != nil {
		return store.EphemeralChildAttestation{}, err
	}
	actor, err := s.store.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return store.EphemeralChildAttestation{}, err
	}
	if !s.steps.awaitConnection(ctx, actor) {
		return store.EphemeralChildAttestation{}, errors.New("ephemeral actor is not connected to the relay")
	}
	attempt, err := s.store.ReserveEphemeralChildAttestation(ctx, intentID, attemptID, authorize)
	if err != nil || attempt.Replay {
		return attempt, err
	}
	fail := func(reason string) (store.EphemeralChildAttestation, error) {
		cleanup, cancel := bookkeeping(ctx)
		defer cancel()
		err := s.store.UnconfirmEphemeralChildAttestation(cleanup, intentID, attemptID, authorize)
		return store.EphemeralChildAttestation{}, errors.Join(errors.New(reason), err)
	}
	claim, err := s.ephemeralAttestor.claims.Issue(attest.Request{ActorUID: attempt.ActorUID, Generation: attempt.ActorIncarnation, Swarm: parent.Principal.SwarmID, Step: strconv.FormatInt(intent.QueuePosition, 10), Origin: attempt.Origin, Commit: attempt.Commit})
	if err != nil {
		return fail("ephemeral workspace attestation challenge is unavailable")
	}
	defer s.ephemeralAttestor.claims.Revoke(claim.Challenge)
	arguments, err := json.Marshal(map[string]string{"swarm": claim.Swarm, "step": claim.Step, "origin": claim.Origin, "commit": claim.Commit, "challenge": claim.Challenge})
	if err != nil {
		return fail("ephemeral workspace attestation request encoding failed")
	}
	check := func(ctx context.Context) error {
		return s.store.CheckEphemeralChildAttestation(ctx, intentID, attemptID, authorize)
	}
	if err := check(ctx); err != nil || ctx.Err() != nil {
		return fail("ephemeral workspace attestation authority changed before the call")
	}
	lease, err := s.store.CurrentActorOperation(ctx, attempt.ActorUID)
	if err != nil {
		return fail("ephemeral workspace attestation lease is unavailable")
	}
	callCtx := s.managed.fenceSessionOpen(ctx, lease, check)
	response, err := hub.CallSession(callCtx, attempt.ActorUID, attempt.ActorIncarnation, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: arguments, Caller: "ephemeral:" + intentID})
	if err != nil || !response.OK || response.Outcome == "unknown" || response.Outcome == "unconfirmed" {
		return fail("ephemeral workspace attestation was not confirmed; the workspace hold is retained")
	}
	proof, err := attest.DecodeAttestation([]byte(response.Text))
	if err != nil || proof.Challenge != claim.Challenge {
		return fail("ephemeral workspace attestation proof is invalid")
	}
	verified, err := s.ephemeralAttestor.claims.Consume(proof, workspaceAttestationKey(s.managed.registry.relaySecret))
	if err != nil {
		return fail("ephemeral workspace attestation proof was rejected")
	}
	if err := s.store.RecordEphemeralChildAttestation(ctx, intentID, attemptID, verified.Origin, verified.Commit, authorize); err != nil {
		return fail("ephemeral workspace attestation proof could not be retained")
	}
	return s.store.GetEphemeralChildAttestation(ctx, intentID)
}
