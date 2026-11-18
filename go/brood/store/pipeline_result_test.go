package store

// FEATURE: Result observation резервируется и сохраняет signed evidence (#290).
// SPEC: Точная identity/pass/attempt и operation lease; owner stop/intervention
// отсекают поздний ответ, unknown не освобождает slot и не повторяется.

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
)

const resultTestToken = "synthetic-result-only-token"

func resultObservationFixture(t *testing.T) (*Store, PipelineCompletionIntent, attest.ResultRequest) {
	t.Helper()
	s, in := completionIntentFixture(t)
	receipt, err := s.AcceptPipelineCompletion(context.Background(), in, completionNonce(), allow)
	mustNil(t, err)
	_, err = s.AcknowledgePipelineCompletion(context.Background(), receiptAck(receipt), allow)
	mustNil(t, err)
	intent := receipt.Intent
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: in.ActorUID, Generation: in.Incarnation, Swarm: in.SwarmID, Step: in.RequestID, Origin: intent.Origin, Commit: intent.BaseCommit, Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}, Pass: intent.Pass, Sequence: in.Sequence}
	return s, intent, request
}
func signedResult(t *testing.T, request attest.ResultRequest) attest.ResultAttestation {
	t.Helper()
	claim := attest.ResultClaim{ResultRequest: request, Head: strings.Repeat("cd", 20), Paths: []string{"go/brood/store/file.go"}}
	signature, err := attest.SignResult(attest.ResultKey(resultTestToken), claim)
	mustNil(t, err)
	return attest.ResultAttestation{ResultClaim: claim, Signature: signature}
}

func TestPipelineResultObservationReservesAndRetainsVerifiedEvidence(t *testing.T) {
	ctx := context.Background()
	s, intent, request := resultObservationFixture(t)
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
	mustNil(t, err)
	if observation.Intent != intent || observation.State != "inflight" || observation.Revision != 1 || observation.Lease.Operation != WorkspaceResultOperation {
		t.Fatalf("bad observation: %+v", observation)
	}
	mustNil(t, s.CheckPipelineResultObservation(ctx, observation))
	signed := signedResult(t, request)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	got, err := s.GetPipelineResultEvidence(ctx, "one", 1)
	mustNil(t, err)
	if !reflect.DeepEqual(got, signed) {
		t.Fatal("evidence lost exact signature or artifact")
	}
	current, err := s.GetPipelineResultObservation(ctx, "one", 1)
	mustNil(t, err)
	if current.State != "observed" || current.Revision != 2 {
		t.Fatalf("observation not settled: %+v", current)
	}
	if _, err := s.CurrentActorOperation(ctx, intent.ActorUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirmed observation retained workspace slot: %v", err)
	}
	if stepCount(t, s) != 1 || stepOf(t, s, 1).State != "inflight" {
		t.Fatal("recording evidence advanced graph")
	}
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
}

func TestPipelineResultObservationRefusesOtherArtifactBindings(t *testing.T) {
	for _, kind := range []string{"actor", "incarnation", "pass", "sequence", "input", "signature"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, _, request := resultObservationFixture(t)
			observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
			mustNil(t, err)
			signed := signedResult(t, request)
			switch kind {
			case "actor":
				signed.ActorUID = "foreign"
			case "incarnation":
				signed.Generation++
			case "pass":
				signed.Pass++
			case "sequence":
				signed.Sequence++
			case "input":
				signed.Commit = strings.Repeat("ef", 20)
			case "signature":
				signed.Signature = "invalid"
			}
			if err := s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed); err == nil {
				t.Fatal("forged or foreign result persisted")
			}
			if _, err := s.GetPipelineResultEvidence(ctx, "one", 1); !errors.Is(err, ErrNotFound) {
				t.Fatalf("forgery created evidence: %v", err)
			}
			current, err := s.CurrentActorOperation(ctx, request.ActorUID)
			mustNil(t, err)
			if current != observation.Lease {
				t.Fatal("forgery released operation")
			}
		})
	}
}

func TestPipelineResultObservationLateReplyCannotCrossOwnerStop(t *testing.T) {
	ctx := context.Background()
	s, _, request := resultObservationFixture(t)
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
	mustNil(t, err)
	stopOne(t, s)
	if err := s.CheckPipelineResultObservation(ctx, observation); err == nil {
		t.Fatal("stopped pass reached result RPC")
	}
	if err := s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signedResult(t, request)); err == nil {
		t.Fatal("late result accepted after owner stop")
	}
	if _, err := s.CurrentActorOperation(ctx, request.ActorUID); err != nil {
		t.Fatal("late result lost uncertain reservation")
	}
}

func TestPipelineResultObservationUnknownKeepsSlotAndDoesNotRetry(t *testing.T) {
	ctx := context.Background()
	s, _, request := resultObservationFixture(t)
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
	mustNil(t, err)
	mustNil(t, s.SettlePipelineResultObservation(ctx, observation, false, "relay result is unknown"))
	current, err := s.GetPipelineResultObservation(ctx, "one", 1)
	mustNil(t, err)
	if current.State != "unconfirmed" {
		t.Fatal("unknown observation became known refusal")
	}
	lease, err := s.CurrentActorOperation(ctx, request.ActorUID)
	mustNil(t, err)
	if lease.State != "unconfirmed" {
		t.Fatal("unknown observation released slot")
	}
	if _, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-2", "swarm:one", request); err == nil {
		t.Fatal("unknown observation retried")
	}
	if err := s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signedResult(t, request)); err == nil {
		t.Fatal("late evidence erased unknown state")
	}
}

func TestPipelineResultKnownRefusalReleasesOnlyItsLease(t *testing.T) {
	ctx := context.Background()
	s, _, request := resultObservationFixture(t)
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
	mustNil(t, err)
	mustNil(t, s.SettlePipelineResultObservation(ctx, observation, true, "workspace is dirty; no verified result"))
	current, err := s.GetPipelineResultObservation(ctx, "one", 1)
	mustNil(t, err)
	if current.State != "refused" || current.Revision != 2 {
		t.Fatal("known refusal became unknown or success")
	}
	if _, err := s.CurrentActorOperation(ctx, request.ActorUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("known settled operation kept slot: %v", err)
	}
	if _, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-2", "swarm:one", request); err == nil {
		t.Fatal("known refusal was silently retried")
	}
}

func TestPipelineResultObservationHonorsOwnerIntervention(t *testing.T) {
	ctx := context.Background()
	s, _, request := resultObservationFixture(t)
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
	mustNil(t, err)
	actor, err := s.GetManagedActor(ctx, request.ActorUID)
	mustNil(t, err)
	_, err = s.AcquireOwnerActorOperation(ctx, actor, "owner-control", "u", "slash", "u", func(context.Context) error { return nil })
	mustNil(t, err)
	if err := s.CheckPipelineResultObservation(ctx, observation); err == nil {
		t.Fatal("owner intervention did not fence result RPC")
	}
	if err := s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signedResult(t, request)); err == nil {
		t.Fatal("late result crossed intervention fence")
	}
	if err := s.SettlePipelineResultObservation(ctx, observation, true, "late known reply"); err == nil {
		t.Fatal("late acknowledgement released interrupted effect")
	}
	mustNil(t, s.SettlePipelineResultObservation(ctx, observation, false, "owner interrupted observation"))
	lease, err := s.CurrentActorOperation(ctx, request.ActorUID)
	mustNil(t, err)
	if lease.State != "unconfirmed" || lease.InterruptedBy != "owner-control" {
		t.Fatal("intervention evidence was erased")
	}
}

func TestPipelineResultObservationRecoveryRetainsOnlyItsOwnUnknownEffects(t *testing.T) {
	ctx := context.Background()
	s, _, request := resultObservationFixture(t)
	_, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
	mustNil(t, err)
	unrelated, err := s.AcquireActorOperation(ctx, "b", 1, "unrelated", "caller", WorkspaceAttestOperation)
	mustNil(t, err)
	mustNil(t, s.RecoverPipelineResultObservations(ctx))
	current, err := s.GetPipelineResultObservation(ctx, "one", 1)
	mustNil(t, err)
	if current.State != "unconfirmed" || current.Revision != 2 {
		t.Fatal("abandoned observation did not become unknown")
	}
	lease, err := s.CurrentActorOperation(ctx, request.ActorUID)
	mustNil(t, err)
	if lease.State != "unconfirmed" {
		t.Fatal("recovery released uncertain workspace")
	}
	other, err := s.CurrentActorOperation(ctx, "b")
	mustNil(t, err)
	if other != unrelated {
		t.Fatal("result recovery changed another operation")
	}
	before := dumpTables(t, s, "pipeline_result_observations", "coordination_claims")
	mustNil(t, s.RecoverPipelineResultObservations(ctx))
	if dumpTables(t, s, "pipeline_result_observations", "coordination_claims") != before {
		t.Fatal("repeated recovery changed unknown history")
	}
}

func TestPipelineResultEvidenceCannotBeRewritten(t *testing.T) {
	ctx := context.Background()
	s, _, request := resultObservationFixture(t)
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "observe-1", "swarm:one", request)
	mustNil(t, err)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signedResult(t, request)))
	for _, query := range []string{`UPDATE pipeline_result_evidence SET attestation='{}'`, `DELETE FROM pipeline_result_evidence`, `INSERT OR REPLACE INTO pipeline_result_evidence SELECT * FROM pipeline_result_evidence`} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "pipeline result evidence is immutable") {
			t.Fatalf("evidence history fence: %v", err)
		}
	}
}
