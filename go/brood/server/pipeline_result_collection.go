package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// collectResult обрабатывает только acknowledged intent без прежнего observation.
// Queue и резерв независимы: последняя проверка повторяется в SSH OpenChannel.
func (d *stepDispatcher) collectResult(ctx context.Context, swarmID string) bool {
	s := d.server
	intent, err := s.store.PendingPipelineResultIntent(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("result observation lookup failed", "swarm", swarmID, "err", err.Error())
		}
		return true
	}
	actor, err := s.store.GetManagedActor(ctx, intent.ActorUID)
	if err != nil {
		slog.Error("result actor lookup failed", "swarm", swarmID, "err", err.Error())
		return true
	}
	actor, namespace, err := s.managed.authorizeCommandActor(ctx, actor)
	if err != nil {
		slog.Info("result actor namespace or authority changed", "swarm", swarmID)
		return true
	}
	request, err := d.results.IssueResult(attest.Request{ActorUID: intent.ActorUID, Generation: intent.Incarnation, Swarm: intent.SwarmID, Step: intent.RequestID, Origin: intent.Origin, Commit: intent.BaseCommit}, intent.Pass, intent.Sequence)
	if err != nil {
		slog.Error("result challenge unavailable", "swarm", swarmID, "err", err.Error())
		return true
	}
	defer d.results.Revoke(request.Challenge)
	var observation store.PipelineResultObservation
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		if err := s.managed.checkResultCollectorAuthority(ctx, actor, intent, namespace.AtespaceName, namespace.AtespaceUID); err != nil {
			return err
		}
		var err error
		observation, err = s.store.ReservePipelineResultObservation(ctx, swarmID, intent.Sequence, store.NewID(), stepMessageSender(swarmID), request)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			slog.Info("result observation reservation refused", "swarm", swarmID, "err", err.Error())
		}
		return true
	}
	settle := func(known bool, reason string) { d.settleResult(ctx, observation, known, reason) }
	hub, ok := s.managed.hub.(sessionRelay)
	if !ok || s.managed.registry == nil || len(s.managed.registry.relaySecret) < 32 {
		settle(true, "result observation unavailable: the Proxmox relay is not configured")
		return true
	}
	if !d.awaitConnection(ctx, actor) {
		settle(true, "result observation was not started: the actor relay is unavailable")
		return true
	}
	check := func(ctx context.Context) error {
		return s.store.WithTx(ctx, func(ctx context.Context) error {
			if err := s.managed.checkResultCollectorAuthority(ctx, actor, intent, namespace.AtespaceName, namespace.AtespaceUID); err != nil {
				return err
			}
			return s.store.CheckPipelineResultObservation(ctx, observation)
		})
	}
	if err := check(ctx); err != nil || ctx.Err() != nil {
		settle(true, "result observation was not started: the attempt or workspace authority changed")
		return true
	}
	args, err := json.Marshal(struct {
		Swarm     string `json:"swarm"`
		Step      string `json:"step"`
		Origin    string `json:"origin"`
		Commit    string `json:"commit"`
		Challenge string `json:"challenge"`
		Pass      int64  `json:"pass"`
		Sequence  int64  `json:"sequence"`
	}{request.Swarm, request.Step, request.Origin, request.Commit, request.Challenge, request.Pass, request.Sequence})
	if err != nil {
		settle(true, "result observation request encoding failed")
		return true
	}
	callCtx := s.managed.fenceSessionOpen(ctx, observation.Lease, check)
	response, err := hub.CallSession(callCtx, actor.UID, actor.Incarnation, relay.SessionRequest{Operation: relay.WorkspaceResultOperation, Arguments: args, Caller: stepMessageSender(swarmID)})
	if err != nil || response.Outcome == "unknown" || response.Outcome == "unconfirmed" {
		settle(false, "result observation outcome is unknown; inspect the retained workspace reservation")
		return true
	}
	_, currentNamespace, authorityErr := s.managed.authorizeCommandActor(ctx, actor)
	if authorityErr != nil || currentNamespace.AtespaceName != namespace.AtespaceName || currentNamespace.AtespaceUID != namespace.AtespaceUID {
		settle(false, "result observation actor namespace or authority changed; the effect remains unconfirmed")
		return true
	}
	if err := check(ctx); err != nil {
		settle(false, "result observation scheduling authority changed; the effect remains unconfirmed")
		return true
	}
	if !response.OK {
		settle(true, "result observation refused by the actor: "+guestText(response.Text))
		return true
	}
	signed, err := attest.DecodeResultAttestation([]byte(response.Text))
	if err != nil || signed.Challenge != request.Challenge {
		settle(false, "result observation returned invalid evidence or another challenge; the effect remains unconfirmed")
		return true
	}
	key := workspaceResultAttestationKey(s.managed.registry.relaySecret)
	if _, err := d.results.ConsumeResult(signed, key); err != nil {
		settle(false, "result observation signature or exact attempt binding was rejected; the effect remains unconfirmed")
		return true
	}
	bctx, cancel := bookkeeping(ctx)
	defer cancel()
	err = s.store.WithTx(bctx, func(ctx context.Context) error {
		if err := s.managed.checkResultCollectorAuthority(ctx, actor, intent, namespace.AtespaceName, namespace.AtespaceUID); err != nil {
			return err
		}
		return s.store.RecordPipelineResultEvidence(ctx, observation, key(request.ActorUID, request.Generation), signed)
	})
	if err != nil {
		settle(false, "result evidence was not recorded because the attempt or owner intervention changed")
	} else {
		d.kick(swarmID)
	}
	return true
}

func (d *stepDispatcher) settleResult(ctx context.Context, observation store.PipelineResultObservation, known bool, reason string) {
	bctx, cancel := bookkeeping(ctx)
	defer cancel()
	err := d.server.store.SettlePipelineResultObservation(bctx, observation, known, reason)
	if err != nil && known {
		err = d.server.store.SettlePipelineResultObservation(bctx, observation, false, "result observation was interrupted; its effect remains unconfirmed")
	}
	if err != nil {
		slog.Error("result observation settlement failed", "swarm", observation.Intent.SwarmID, "observation", observation.ID, "err", err.Error())
	} else if known {
		// Settlement уже committed; новый проход делает только policy bookkeeping,
		// не повторную observation и не подмену сообщения актора.
		d.kick(observation.Intent.SwarmID)
	}
}
