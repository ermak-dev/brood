package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Immutable-привязка эфемерного child читается без живого actor inventory (#292).
// SPEC: Точные UID/incarnation или proof-bound child ID, без новых прав и записей;
// обычный ad-hoc child не становится эфемерным, удалённый actor не теряет историю.
// IMPLEMENTATION: Один snapshot-запрос по immutable allocation или proof-bound child.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralHistoryLookupsKeepExactIdentityWithoutWrites(t *testing.T) {
	ctx := context.Background()
	s, intent := registrationFixture(t, 2)
	before := databaseRows(t, s.db)
	got, err := s.GetEphemeralChildIntentForActor(ctx, "isolated", 1)
	mustNil(t, err)
	if got != intent || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("actor lookup changed immutable intent or wrote history")
	}
	for _, identity := range []struct {
		uid         string
		incarnation int64
	}{{"isolated", 2}, {"b", 1}, {"", 1}, {"isolated", 0}} {
		got, err := s.GetEphemeralChildIntentForActor(ctx, identity.uid, identity.incarnation)
		if !errors.Is(err, ErrNotFound) || got != (EphemeralChildIntent{}) {
			t.Fatalf("foreign actor mapped to intent: %+v %v", got, err)
		}
	}
	child, err := s.RegisterEphemeralChild(ctx, intent.ID, childAllow)
	mustNil(t, err)
	executor := intent.Parent.Principal
	executor.ActorUID = "b"
	legacy, err := s.ReserveSwarmChild(ctx, intent.Parent, "legacy", "Legacy ad-hoc task", executor, childAllow)
	mustNil(t, err)
	before = databaseRows(t, s.db)
	got, err = s.GetEphemeralChildIntentForTask(ctx, child.ID)
	mustNil(t, err)
	if got != intent {
		t.Fatal("task lookup lost immutable source or parent")
	}
	for _, id := range []string{legacy.ID, "missing", ""} {
		got, err := s.GetEphemeralChildIntentForTask(ctx, id)
		if !errors.Is(err, ErrNotFound) || got != (EphemeralChildIntent{}) {
			t.Fatalf("ordinary or absent child gained ephemeral history: %+v %v", got, err)
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("history reads changed rows")
	}
}

func TestEphemeralActorHistoryAvailableBeforeReadyOrAttestation(t *testing.T) {
	ctx := context.Background()
	s, intent := ephemeralAttestationFixture(t, false)
	before := databaseRows(t, s.db)
	got, err := s.GetEphemeralChildIntentForActor(ctx, "isolated", 1)
	mustNil(t, err)
	if got != intent || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("bootstrap history depended on ready state or wrote rows")
	}
	if _, err := s.GetEphemeralChildAttestation(ctx, intent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("history lookup fabricated attestation: %v", err)
	}
}

func TestEphemeralHistorySurvivesInventoryRemovalAfterRetainedReport(t *testing.T) {
	ctx := context.Background()
	s, intent := registrationFixture(t, 1)
	child, err := s.RegisterEphemeralChild(ctx, intent.ID, childAllow)
	mustNil(t, err)
	_, err = s.PrepareEphemeralChildDelivery(ctx, intent.ID, ActorDeliveryInput{SwarmID: child.Executor.SwarmID, SourceKind: "child", SourceID: child.ID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: child.Executor.ActorUID, Incarnation: child.Executor.Incarnation, AtespaceUID: child.Executor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"body":"retained instruction"}`}, childAllow)
	mustNil(t, err)
	join, err := s.GetSwarmChildJoin(ctx, intent.Parent, childAllow)
	mustNil(t, err)
	child = join.Children[0]
	child, err = s.CompleteSwarmChild(ctx, intent.Parent, intent.RequestID, child.Executor, child.Revision, "success", "Result report retained in the server database.", childAllow)
	mustNil(t, err)
	// Моделируется только отсутствие inventory, не подтверждение физического cleanup.
	_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid='isolated'`)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	byActor, err := s.GetEphemeralChildIntentForActor(ctx, "isolated", 1)
	mustNil(t, err)
	byTask, err := s.GetEphemeralChildIntentForTask(ctx, child.ID)
	mustNil(t, err)
	if byActor != intent || byTask != intent || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("inventory removal lost or rewrote retained child history")
	}
}
