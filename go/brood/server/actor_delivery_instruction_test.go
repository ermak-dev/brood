package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type durableInstructionRelay struct {
	commandSessionHub
	actor   store.ManagedActor
	journal string
}

func (h durableInstructionRelay) DeliveryJournalID(uid string, generation int64) string {
	if uid == h.actor.UID && generation == h.actor.Incarnation {
		return h.journal
	}
	return ""
}

func durableDispatchServer(t *testing.T) (*dispatchServer, *atomic.Int64) {
	t.Helper()
	d := newDispatchServer(t)
	d.srv.actorDelivery.close()
	clock := &atomic.Int64{}
	clock.Store(time.Now().UTC().UnixNano())
	d.srv.actorDelivery = newActorDeliveryService(d.srv, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	d.srv.managed.hub = durableInstructionRelay{commandSessionHub: d.srv.managed.hub.(commandSessionHub), actor: d.worker, journal: strings.Repeat("a", 64)}
	return d, clock
}

func TestDurableInstructionCommitsAttestationPayloadAndTicketBeforeMessage(t *testing.T) {
	d, _ := durableDispatchServer(t)
	d.deliver = func(call deliveredMessage) (relay.MessageResponse, error) {
		intent, err := d.srv.store.GetActorDeliveryForSource(context.Background(), d.swarm, "step", "1")
		if err != nil {
			t.Errorf("instruction sent before payload commit: %v", err)
			return relay.MessageResponse{}, err
		}
		history, err := d.srv.store.GetActorDeliveryHistory(context.Background(), intent.ID)
		if err != nil || len(history) != 1 || history[0].Attempt.Mode != "send" {
			t.Error("instruction sent before durable ticket")
		}
		if call.Step.State != "delivering" || call.Step.AttestedCommit != dispatchCommit || !d.leaseReleased(d.worker) {
			t.Error("instruction escaped its committed attestation boundary")
		}
		if call.Request.DeliveryID != intent.ID || call.Request.JournalID != intent.Input.JournalID || call.Request.Replay {
			t.Error("instruction lost stable delivery scope")
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	d.start("deliver")
	attests, messages := d.calls()
	if len(attests) != 1 || len(messages) != 1 || d.last().State != "inflight" || messages[0].Request.DeliveryID == "" {
		t.Fatal("capable instruction did not use retained delivery")
	}
}

func TestDurableInstructionRecoveryNeverRepeatsAttestationOrModelWrite(t *testing.T) {
	d, clock := durableDispatchServer(t)
	writes := 0
	d.deliver = func(call deliveredMessage) (relay.MessageResponse, error) {
		if !call.Request.Replay {
			writes++
			return relay.MessageResponse{}, errors.New("receipt lost")
		}
		return relay.MessageResponse{OK: true, Duplicate: true, Verdict: "prior receipt confirmed"}, nil
	}
	d.start("deliver")
	before := d.last()
	if before.State != "unconfirmed" {
		t.Fatalf("lost instruction receipt did not stay unknown: %+v", before)
	}
	clock.Add(int64(time.Hour))
	if _, err := d.srv.actorDelivery.recoverSteps(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	attests, messages := d.calls()
	after := d.last()
	if len(attests) != 1 || len(messages) != 2 || writes != 1 || after.State != "inflight" || after.RequestID != before.RequestID || after.AttestedCommit != before.AttestedCommit || len(d.steps()) != 1 {
		t.Fatal("recovery repeated preparation or changed the logical step")
	}
	first, probe := messages[0].Request, messages[1].Request
	if !probe.Replay || first.DeliveryID == "" || first.DeliveryID != probe.DeliveryID || first.Body != probe.Body || first.JournalID != probe.JournalID {
		t.Fatal("instruction recovery changed identity or resent payload")
	}
}

func TestDurableInstructionKnownNotSentUsesPersistedBackoffNotLegacyLoop(t *testing.T) {
	d, clock := durableDispatchServer(t)
	d.deliver = func(call deliveredMessage) (relay.MessageResponse, error) {
		_, messages := d.calls()
		if len(messages) == 1 {
			return sessionNotReady, nil
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	d.start("deliver")
	_, messages := d.calls()
	if len(messages) != 1 || d.last().State != "delivering" {
		t.Fatal("capable instruction used legacy resend loop or lost not-sent evidence")
	}
	clock.Add(int64(time.Hour))
	if _, err := d.srv.actorDelivery.recoverSteps(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	attests, messages := d.calls()
	if len(attests) != 1 || len(messages) != 2 || messages[1].Request.Replay || messages[0].Request.DeliveryID != messages[1].Request.DeliveryID || d.last().State != "inflight" {
		t.Fatal("safe instruction retry changed identity or repeated attestation")
	}
}

func TestDurableInstructionRecoveryDoesNotFallbackAfterStopOrJournalChange(t *testing.T) {
	for _, change := range []string{"stop", "journal"} {
		t.Run(change, func(t *testing.T) {
			d, clock := durableDispatchServer(t)
			d.deliver = func(deliveredMessage) (relay.MessageResponse, error) {
				return relay.MessageResponse{}, errors.New("receipt lost")
			}
			d.start("deliver")
			if change == "stop" {
				d.stop()
			} else {
				hub := d.srv.managed.hub.(durableInstructionRelay)
				hub.journal = strings.Repeat("b", 64)
				d.srv.managed.hub = hub
			}
			clock.Add(int64(time.Hour))
			if _, err := d.srv.actorDelivery.recoverSteps(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			attests, messages := d.calls()
			if len(attests) != 1 || len(messages) != 1 || d.last().State != "unconfirmed" {
				t.Fatal("stopped/changed instruction escaped retained recovery scope")
			}
		})
	}
}

func TestInstructionPreparationRejectsMalformedJournalIdentity(t *testing.T) {
	for _, journal := range []string{"short", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, err := preparedInstructionDelivery(store.ActorStepAttempt{SwarmID: "s", Sequence: 1, RequestID: "request", Actor: store.ManagedActor{UID: "a", Name: "a"}}, "work", journal); err == nil {
			t.Fatal("invalid capability identity reached attestation preparation")
		}
	}
}

func TestInstructionMetadataSizeIsCheckedBeforeAttestation(t *testing.T) {
	d, _ := durableDispatchServer(t)
	d.startPending("deliver")
	a, err := d.srv.store.ReserveActorStep(context.Background(), d.swarm, store.NewID(), stepMessageSender(d.swarm))
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := json.Marshal(relay.MessageRequest{To: a.Actor.Name, From: stepMessageSender(a.SwarmID), Body: ""})
	text := strings.Repeat("x", relay.MaxMessageBytes-len(empty)-1)
	legacy, _ := json.Marshal(relay.MessageRequest{To: a.Actor.Name, From: stepMessageSender(a.SwarmID), Body: text})
	if len(legacy) > relay.MaxMessageBytes {
		t.Fatal("test did not fit legacy relay frame")
	}
	if _, err := preparedInstructionDelivery(a, text, strings.Repeat("a", 64)); err == nil {
		t.Fatal("durable metadata bypassed relay frame bound")
	}
	if d.srv.steps.attest(context.Background(), a, text) {
		t.Fatal("oversized durable frame opened workspace attestation")
	}
	attests, messages := d.calls()
	if len(attests) != 0 || len(messages) != 0 || d.last().State != "refused" || !d.leaseReleased(d.worker) {
		t.Fatal("size refusal happened after guest effect or leaked its lease")
	}
}
