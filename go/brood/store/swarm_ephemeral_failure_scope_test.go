package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// FEATURE: Локальный отказ сохраняет исходный job и unknown physical inventory.
// SPEC: История доступна после потери authority, новый job не наследует отказ;
// отсутствие proof никогда не выдаёт право cleanup или освобождение capacity.
func TestEphemeralFailureBeforeReadyKeepsCapacityAndHistory(t *testing.T) {
	ctx := context.Background()
	s, job := ephemeralAttestationFixture(t, false)
	before, err := s.GetManagedActor(ctx, "isolated")
	mustNil(t, err)
	auth := func(_ context.Context, a SwarmChildAuthorization) error {
		want := job.Parent.Principal
		want.ActorUID = "isolated"
		want.Incarnation = 1
		if a.Executor != want || a.Parent != job.Parent {
			t.Fatalf("allocated failure lost exact scope: %+v", a)
		}
		return nil
	}
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, "Explicit local refusal before bootstrap", auth))
	failure, err := s.GetEphemeralChildFailure(ctx, job.ID)
	mustNil(t, err)
	if failure.IntentID != job.ID || failure.RequestID != job.RequestID || failure.Reason != "Explicit local refusal before bootstrap" {
		t.Fatalf("failure history mismatch: %+v", failure)
	}
	_, err = time.Parse(time.RFC3339Nano, failure.FailedAt)
	mustNil(t, err)
	after, err := s.GetManagedActor(ctx, "isolated")
	mustNil(t, err)
	if before != after || after.Stage != "create" || after.Phase != "provisioning" || after.UPID != "" {
		t.Fatal("logical failure claimed unknown Create was terminal")
	}
	_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
	mustNil(t, err)
	// Обычный admission всё ещё видит физически зарезервированный слот.
	_, err = s.ReserveManagedActor(ctx, managedRequest("cannot-fit", "one", 304))
	if !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("failure/join freed physical capacity: %v", err)
	}
	history, err := s.GetEphemeralChildHistory(ctx, job.ID)
	mustNil(t, err)
	if history.Failure == nil || *history.Failure != failure || history.Allocation == nil || history.Attestation != nil || history.ResultSeal != nil || history.ChildID != "" {
		t.Fatalf("allocated history conflated fact types: %+v", history)
	}
}

func TestEphemeralFailureRetryRequiresCurrentAuthority(t *testing.T) {
	ctx := context.Background()
	s, job := allocationFixture(t, 1)
	reason := "Retained decision"
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, reason, childAllow))
	before := databaseRows(t, s.db)
	if err := recordUnregisteredFailure(s, ctx, job.ID, reason, nil); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("retry skipped mandatory authority: %v", err)
	}
	setActor(t, s, job.Parent.Principal.ActorUID, "desired='stopped'")
	stale := databaseRows(t, s.db)
	if err := recordUnregisteredFailure(s, ctx, job.ID, reason, childAllow); err == nil {
		t.Fatal("stale parent reauthorized retained fact")
	}
	f, err := s.GetEphemeralChildFailure(ctx, job.ID)
	mustNil(t, err)
	original, err := s.GetEphemeralChildIntent(ctx, job.ID)
	mustNil(t, err)
	if original != job || f.Reason != reason || !reflect.DeepEqual(stale, databaseRows(t, s.db)) || !reflect.DeepEqual(before["ephemeral_child_failures"], stale["ephemeral_child_failures"]) {
		t.Fatal("stale retry destroyed immutable history")
	}
}

func TestEphemeralFailureUnknownLeaseStillHoldsAfterJoin(t *testing.T) {
	ctx := context.Background()
	s, job := ephemeralAttestationFixture(t, true)
	_, err := s.ReserveEphemeralChildAttestation(ctx, job.ID, "pending", childAllow)
	mustNil(t, err)
	lease, err := s.CurrentActorOperation(ctx, "isolated")
	mustNil(t, err)
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, "Local rejection, not an observed RPC failure", childAllow))
	_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
	mustNil(t, err)
	current, err := s.CurrentActorOperation(ctx, "isolated")
	mustNil(t, err)
	if current != lease {
		t.Fatal("join/failure rewrote pending workspace lease")
	}
	_, err = s.AcquireActorOperation(ctx, "isolated", 1, "new-work", "server", "workspace_attest")
	if err == nil {
		t.Fatal("logical failure granted new workspace access over pending effect")
	}
}
