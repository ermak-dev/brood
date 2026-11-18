package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestActorDeliveryDuplicateReconcilesOnlyItsAnsweredRequest(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	before, err := s.GetSwarmRequest(ctx, in.SwarmID, in.SourceID)
	mustNil(t, err)
	steps := stepStates(t, s)
	now := time.Now().UTC()
	first, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, first, "unconfirmed", "reply lost", now, allow))
	_, err = s.RecordSwarmRequestDelivery(ctx, in.SwarmID, in.SourceID, "unconfirmed")
	mustNil(t, err)
	if err := s.ReconcileActorDelivery(ctx, intent.ID, allow); !errors.Is(err, ErrActorDeliveryReceiptPending) {
		t.Fatal("source reconciled without retained confirmation")
	}
	now = now.Add(time.Hour)
	probe, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, probe, "duplicate", "prior delivery confirmed", now, allow))
	mustNil(t, s.ReconcileActorDelivery(ctx, intent.ID, allow))
	mustNil(t, s.ReconcileActorDelivery(ctx, intent.ID, allow))
	after, err := s.GetSwarmRequest(ctx, in.SwarmID, in.SourceID)
	mustNil(t, err)
	before.Delivery = "delivered"
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(steps, stepStates(t, s)) {
		t.Fatal("duplicate reconciliation changed the answer or graph")
	}
}

func confirmedStepDelivery(t *testing.T) (*Store, ActorStepAttempt, ActorDeliveryIntent) {
	t.Helper()
	ctx := context.Background()
	s := dispatchStore(t)
	a := attestOne(t, s, "retained-delivery")
	mustNil(t, s.FinishActorOperation(ctx, a.Lease, true))
	in := ActorDeliveryInput{SwarmID: "one", SourceKind: "step", SourceID: "1", RequestID: a.RequestID, Commit: startCommit, ActorUID: a.Actor.UID, Incarnation: a.Actor.Incarnation, AtespaceUID: a.Actor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"to":"a","from":"swarm:one","body":"build","waitSec":0}`}
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	now := time.Now().UTC()
	first, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, first, "unconfirmed", "reply lost", now, allow))
	mustNil(t, s.UnconfirmActorStep(ctx, a, "reply lost"))
	now = now.Add(time.Hour)
	probe, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, probe, "duplicate", "prior delivery confirmed", now, allow))
	return s, a, intent
}

func TestActorDeliveryStepReconciliationKeepsSourceAndDoesNotReviveAttestationLease(t *testing.T) {
	ctx := context.Background()
	s, a, intent := confirmedStepDelivery(t)
	mustNil(t, s.ReconcileActorDelivery(ctx, intent.ID, allow))
	mustNil(t, s.ReconcileActorDelivery(ctx, intent.ID, allow))
	visit := stepOf(t, s, 1)
	if visit.State != "inflight" || visit.RequestID != a.RequestID || visit.AttestedCommit != startCommit || visit.ActorUID != a.Actor.UID || stepCount(t, s) != 1 {
		t.Fatalf("duplicate changed the logical visit: %+v", visit)
	}
	if _, err := s.CurrentActorOperation(ctx, a.Actor.UID); !errors.Is(err, ErrNotFound) {
		t.Fatal("reconciliation revived the released attestation lease")
	}
}

func TestActorDeliveryReconciliationRefusesRevokedRoleOrStoppedSource(t *testing.T) {
	for _, change := range []string{"authority", "role", "namespace", "stop"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, _, intent := confirmedStepDelivery(t)
			authorize := allow
			switch change {
			case "authority":
				authorize = func(context.Context) bool { return false }
			case "role":
				mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b", ActorIncarnation: 1}))
			case "namespace":
				_, err := s.db.Exec(`UPDATE swarms SET atespace_uid='replacement' WHERE id='one'`)
				mustNil(t, err)
			case "stop":
				_, err := s.StopSwarm(ctx, "one", "u", allow)
				mustNil(t, err)
			}
			if err := s.ReconcileActorDelivery(ctx, intent.ID, authorize); err == nil {
				t.Fatal("retained confirmation bypassed current source authority")
			}
			if stepOf(t, s, 1).State != "unconfirmed" || stepCount(t, s) != 1 {
				t.Fatal("revoked reconciliation changed source")
			}
		})
	}
}

func TestActorDeliveryManagerCheckUsesTransactionContext(t *testing.T) {
	s, _ := actorDeliveryFixture(t)
	for _, user := range []string{"u", "ed", "adm"} {
		mustNil(t, s.WithDurableTx(context.Background(), func(ctx context.Context) error { return s.CheckActorDeliveryManager(ctx, "s", user) }))
	}
	for _, user := range []string{"vw", "out", "missing"} {
		if err := s.CheckActorDeliveryManager(context.Background(), "s", user); err == nil {
			t.Fatalf("%s gained delivery authority", user)
		}
	}
	mustNil(t, s.CreateUser(context.Background(), User{ID: "standby-admin", Username: "standby-admin", PasswordHash: "h", IsAdmin: true}))
	mustNil(t, s.SetUserDisabled(context.Background(), "adm", true))
	if err := s.CheckActorDeliveryManager(context.Background(), "s", "adm"); err == nil {
		t.Fatal("disabled administrator retained delivery authority")
	}
}
