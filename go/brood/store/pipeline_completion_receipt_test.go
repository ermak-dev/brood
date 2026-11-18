package store

// FEATURE: Durable acceptance receipt до admission result observation (#290).
// SPEC: Exact retry сохраняет nonce; ACK точной identity идемпотентен, owner stop
// запрещает поздний первый ACK; без ACK нельзя резервировать result RPC.

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
)

func completionNonce() string { return base64.RawURLEncoding.EncodeToString(make([]byte, 32)) }
func receiptAck(receipt PipelineCompletionReceipt) PipelineCompletionAcknowledgement {
	in := receipt.Intent
	return PipelineCompletionAcknowledgement{Version: receipt.Version, SwarmID: in.SwarmID, Pass: in.Pass, Sequence: in.Sequence, RequestID: in.RequestID, ActorUID: in.ActorUID, Incarnation: in.Incarnation, Nonce: receipt.Nonce}
}
func TestCompletionAcceptancePinsNonceAndRequiresClientAcknowledgement(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	receipt, err := s.AcceptPipelineCompletion(ctx, in, completionNonce(), allow)
	mustNil(t, err)
	if receipt.Version != 1 || receipt.Nonce != completionNonce() || receipt.CreatedAt == "" || receipt.AcknowledgedAt != "" {
		t.Fatalf("invalid initial receipt: %+v", receipt)
	}
	otherNonce := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	retry, err := s.AcceptPipelineCompletion(ctx, in, otherNonce, allow)
	mustNil(t, err)
	if !reflect.DeepEqual(retry, receipt) {
		t.Fatal("exact retry changed receipt nonce or history")
	}
	intent := receipt.Intent
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: in.ActorUID, Generation: in.Incarnation, Swarm: in.SwarmID, Step: in.RequestID, Origin: intent.Origin, Commit: intent.BaseCommit, Challenge: completionNonce()}, Pass: intent.Pass, Sequence: in.Sequence}
	if _, err := s.ReservePipelineResultObservation(ctx, in.SwarmID, in.Sequence, "observe-before-ack", "caller", request); !errors.Is(err, ErrPipelineCompletionReceiptPending) {
		t.Fatalf("unacknowledged acceptance reached result reservation: %v", err)
	}
	confirmed, err := s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	if confirmed.AcknowledgedAt == "" || confirmed.Nonce != receipt.Nonce {
		t.Fatal("ACK did not retain receipt identity")
	}
	again, err := s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	if !reflect.DeepEqual(again, confirmed) {
		t.Fatal("duplicate ACK rewrote timestamp")
	}
	if _, err := s.ReservePipelineResultObservation(ctx, in.SwarmID, in.Sequence, "observe-after-ack", "caller", request); err != nil {
		t.Fatalf("confirmed acceptance did not admit result reservation: %v", err)
	}
}

func TestCompletionAcknowledgementRejectsWrongIdentityScopeAndNonce(t *testing.T) {
	for _, kind := range []string{"nonce", "actor", "incarnation", "swarm", "pass", "sequence", "attempt", "session", "stopped"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, in := completionIntentFixture(t)
			receipt, err := s.AcceptPipelineCompletion(ctx, in, completionNonce(), allow)
			mustNil(t, err)
			ack := receiptAck(receipt)
			authorize := allow
			switch kind {
			case "nonce":
				ack.Nonce = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
			case "actor":
				ack.ActorUID = "b"
			case "incarnation":
				ack.Incarnation++
			case "swarm":
				ack.SwarmID = "foreign"
			case "pass":
				ack.Pass++
			case "sequence":
				ack.Sequence++
			case "attempt":
				ack.RequestID = "foreign"
			case "session":
				authorize = func(context.Context) bool { return false }
			case "stopped":
				stopOne(t, s)
			}
			if _, err := s.AcknowledgePipelineCompletion(ctx, ack, authorize); err == nil {
				t.Fatal("foreign, revoked or late ACK accepted")
			}
			unchanged, err := s.GetPipelineCompletionReceipt(ctx, in.SwarmID, in.Sequence)
			mustNil(t, err)
			if unchanged.AcknowledgedAt != "" {
				t.Fatal("refused ACK changed receipt")
			}
		})
	}
}

func TestCompletionReceiptAndAcknowledgementAreImmutable(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	receipt, err := s.AcceptPipelineCompletion(ctx, in, completionNonce(), allow)
	mustNil(t, err)
	_, err = s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	for _, table := range []string{"pipeline_completion_receipts", "pipeline_completion_acknowledgements"} {
		for _, query := range []string{"UPDATE " + table + " SET sequence=99", "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table} {
			if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "completion receipt history is immutable") {
				t.Fatalf("receipt history fence: %v", err)
			}
		}
	}
}

func TestCompletionReceiptSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	receipt, err := s.AcceptPipelineCompletion(ctx, in, completionNonce(), allow)
	mustNil(t, err)
	receipt, err = s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	var sequence int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	got, err := reopened.GetPipelineCompletionReceipt(ctx, in.SwarmID, in.Sequence)
	mustNil(t, err)
	if !reflect.DeepEqual(got, receipt) {
		t.Fatal("reopen changed durable receipt")
	}
}
