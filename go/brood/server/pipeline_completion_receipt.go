package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func newPipelineCompletionNonce() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func completionReceiptMetadata(receipt store.PipelineCompletionReceipt) relay.CompletionReceipt {
	intent := receipt.Intent
	return relay.CompletionReceipt{Version: receipt.Version, SwarmID: intent.SwarmID, Pass: intent.Pass, Sequence: intent.Sequence, RequestID: intent.RequestID, Nonce: receipt.Nonce}
}

func (m *managedProvider) currentActorReceiptAuthority(ctx context.Context, actor store.ManagedActor) bool {
	_, _, err := m.authorizeCommandActorDurable(ctx, actor)
	return err == nil
}

func (m *managedProvider) acknowledgePipelineCompletion(ctx context.Context, from store.ManagedActor, request relay.SwarmRequest) relay.SwarmResponse {
	receipt, err := relay.DecodeCompletionReceiptAck(request)
	if err != nil || receipt.SwarmID != from.SwarmID {
		return relay.SwarmResponse{Text: "completion receipt acknowledgement refused", Outcome: "refused"}
	}
	err = m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
		_, err := m.server.store.AcknowledgePipelineCompletion(ctx, store.PipelineCompletionAcknowledgement{Version: receipt.Version, SwarmID: current.SwarmID, Pass: receipt.Pass, Sequence: receipt.Sequence, RequestID: receipt.RequestID, ActorUID: current.UID, Incarnation: current.Incarnation, Nonce: receipt.Nonce}, func(ctx context.Context) bool { return m.currentActorReceiptAuthority(ctx, current) })
		return err
	})
	if err != nil {
		return relay.SwarmResponse{Text: "completion receipt acknowledgement refused; the current attempt or actor authority changed", Outcome: "refused"}
	}
	m.server.steps.kick(from.SwarmID)
	return relay.SwarmResponse{OK: true, Text: "completion acceptance receipt recorded; result evidence and policy remain pending", Outcome: "confirmed"}
}
