package server

import (
	"errors"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestDurableDeliveryEvidenceNeverTurnsProbeOrContradictionIntoSendProof(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		response   relay.MessageResponse
		err        error
		want       string
	}{
		{"delivered", "send", relay.MessageResponse{OK: true, Verdict: "delivered"}, nil, "delivered"},
		{"cached", "send", relay.MessageResponse{OK: true, Duplicate: true, Verdict: "confirmed"}, nil, "duplicate"},
		{"not sent", "send", relay.MessageResponse{NotSent: true, Verdict: "not ready"}, nil, "not_sent"},
		{"unknown", "send", relay.MessageResponse{Verdict: "unknown"}, nil, "unconfirmed"},
		{"contradiction", "send", relay.MessageResponse{OK: true, NotSent: true, Verdict: "bad"}, nil, "unconfirmed"},
		{"false duplicate", "send", relay.MessageResponse{Duplicate: true, Verdict: "bad"}, nil, "unconfirmed"},
		{"empty verdict", "send", relay.MessageResponse{OK: true}, nil, "unconfirmed"},
		{"transport failure", "send", relay.MessageResponse{OK: true, Verdict: "untrusted"}, errors.New("channel failed"), "unconfirmed"},
		{"probe receipt", "probe", relay.MessageResponse{OK: true, Duplicate: true, Verdict: "confirmed"}, nil, "duplicate"},
		{"probe no duplicate", "probe", relay.MessageResponse{OK: true, Verdict: "delivered"}, nil, "unconfirmed"},
		{"probe old not sent", "probe", relay.MessageResponse{NotSent: true, Verdict: "old refusal"}, nil, "unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := actorDeliveryEvidence(tc.mode, tc.response, tc.err)
			if got != tc.want {
				t.Fatalf("outcome=%s want%s", got, tc.want)
			}
		})
	}
}
