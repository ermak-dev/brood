package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

// FEATURE: Separate supervisor inbox custody and dispatch permission (#291).
// SPEC: Bind one receiver to an exact, never-attempted delivery. Custody is
// not native confirmation, and dispatch permission is issued at most once.
// IMPLEMENTATION: Separate append-only evidence with reciprocal SQL fences
// and a FULL-sync permit transaction that checks the current launch.

const inboxPositiveCustody = "admitted_to_supervisor_inbox"

func actorDeliveryInboxTarget(intent ActorDeliveryIntent) ActorDeliveryInboxTarget {
	in := intent.Input
	return ActorDeliveryInboxTarget{
		DeliveryID: intent.ID, SwarmID: in.SwarmID, SourceKind: in.SourceKind, SourceID: in.SourceID,
		ActorUID: in.ActorUID, Incarnation: in.Incarnation, ReceiverID: strings.Repeat("b", 64),
		Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(in.Payload))),
	}
}

func actorDeliveryInboxFixture(t *testing.T) (*Store, ActorDeliveryInboxTarget) {
	t.Helper()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(context.Background(), in, allow)
	mustNil(t, err)
	return s, actorDeliveryInboxTarget(intent)
}

func TestActorDeliveryInboxStatusSeparatesAuthorityFromHistory(t *testing.T) {
	ctx := context.Background()
	s, target := actorDeliveryInboxFixture(t)
	if state, err := s.ActorDeliveryInboxStatus(ctx, target.DeliveryID); !errors.Is(err, ErrNotFound) || state != (ActorDeliveryInboxState{}) {
		t.Fatalf("unselected intent obtained inbox status: %+v %v", state, err)
	}
	mustNil(t, s.SelectActorDeliveryInbox(ctx, target, allow))
	if state, err := s.ActorDeliveryInboxStatus(ctx, target.DeliveryID); err != nil || state != (ActorDeliveryInboxState{Target: target}) {
		t.Fatalf("selection invented custody or a permit: %+v %v", state, err)
	}
	mustNil(t, s.RecordActorDeliveryInboxCustody(ctx, target, ActorDeliveryInboxAdmitted, allow))
	if state, err := s.ActorDeliveryInboxStatus(ctx, target.DeliveryID); err != nil || state != (ActorDeliveryInboxState{Target: target, Custody: true}) {
		t.Fatalf("custody was not separate from dispatch: %+v %v", state, err)
	}
	permit, err := s.ReserveActorDeliveryInboxDispatch(ctx, target, "launch-1", func(context.Context, ActorDeliveryInboxTarget, string) bool { return true })
	mustNil(t, err)
	if permit == "" {
		t.Fatal("reservation did not give a one-time permit")
	}
	if state, err := s.ActorDeliveryInboxStatus(ctx, target.DeliveryID); err != nil || state != (ActorDeliveryInboxState{Target: target, Custody: true, DispatchReserved: true}) {
		t.Fatalf("retained status lost the issued-permit mark: %+v %v", state, err)
	}
}

func TestActorDeliveryInboxSelectionIsExactAndImmutable(t *testing.T) {
	ctx := context.Background()
	s, target := actorDeliveryInboxFixture(t)
	intent, err := s.GetActorDelivery(ctx, target.DeliveryID)
	mustNil(t, err)

	for field, change := range map[string]func(*ActorDeliveryInboxTarget){
		"delivery":       func(v *ActorDeliveryInboxTarget) { v.DeliveryID = "" },
		"swarm":          func(v *ActorDeliveryInboxTarget) { v.SwarmID = "other" },
		"source kind":    func(v *ActorDeliveryInboxTarget) { v.SourceKind = "step" },
		"source id":      func(v *ActorDeliveryInboxTarget) { v.SourceID = "other" },
		"actor":          func(v *ActorDeliveryInboxTarget) { v.ActorUID = "other" },
		"incarnation":    func(v *ActorDeliveryInboxTarget) { v.Incarnation++ },
		"payload digest": func(v *ActorDeliveryInboxTarget) { v.Digest = strings.Repeat("c", 64) },
		"empty receiver": func(v *ActorDeliveryInboxTarget) { v.ReceiverID = "" },
		"legacy journal": func(v *ActorDeliveryInboxTarget) { v.ReceiverID = intent.Input.JournalID },
	} {
		t.Run(field, func(t *testing.T) {
			wrong := target
			change(&wrong)
			if err := s.SelectActorDeliveryInbox(ctx, wrong, allow); !errors.Is(err, ErrActorDeliveryConflict) {
				t.Fatalf("selection did not reject changed %s: %v", field, err)
			}
		})
	}
	if err := s.SelectActorDeliveryInbox(ctx, target, nil); !errors.Is(err, ErrActorDeliveryDenied) {
		t.Fatalf("selection without authority: %v", err)
	}
	mustNil(t, s.SelectActorDeliveryInbox(ctx, target, allow))
	mustNil(t, s.SelectActorDeliveryInbox(ctx, target, allow))
	replacement := target
	replacement.ReceiverID = strings.Repeat("d", 64)
	if err := s.SelectActorDeliveryInbox(ctx, replacement, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("selection rebound a retained receiver: %v", err)
	}
	retained, err := s.GetActorDelivery(ctx, target.DeliveryID)
	mustNil(t, err)
	if retained != intent {
		t.Fatal("inbox selection rewrote the retained native delivery intent")
	}
	now := time.Now().UTC()
	if ticket, err := s.ReserveActorDeliveryAttempt(ctx, target.DeliveryID, now, now.Add(time.Minute), allow); ticket != (ActorDeliveryAttempt{}) || !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("inbox-selected delivery still granted a native attempt: %+v, %v", ticket, err)
	}
	for _, mode := range []string{"send", "probe"} {
		if _, err := s.db.Exec(`INSERT INTO actor_delivery_attempts VALUES(?,1,?,?,?)`, target.DeliveryID, mode,
			now.Format(time.RFC3339Nano), now.Add(time.Minute).Format(time.RFC3339Nano)); err == nil {
			t.Fatalf("storage fence accepted a native %s for the selected inbox", mode)
		}
	}
}

func TestActorDeliveryInboxSelectionRejectsNativeOrPreexistingUncertainty(t *testing.T) {
	ctx := context.Background()
	t.Run("known not sent still counts as a reservation", func(t *testing.T) {
		s, target := actorDeliveryInboxFixture(t)
		now := time.Now().UTC()
		ticket, err := s.ReserveActorDeliveryAttempt(ctx, target.DeliveryID, now, now.Add(time.Minute), allow)
		mustNil(t, err)
		mustNil(t, s.RecordActorDeliveryOutcome(ctx, ticket, "not_sent", "not ready", now, allow))
		if err := s.SelectActorDeliveryInbox(ctx, target, allow); !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatalf("inbox selected after a native reservation: %v", err)
		}
		if _, err := s.db.Exec(`INSERT INTO actor_delivery_inbox_selections(delivery_id,receiver_id,digest,selected_at) VALUES(?,?,?,?)`,
			target.DeliveryID, target.ReceiverID, target.Digest, now.Format(time.RFC3339Nano)); err == nil || !strings.Contains(err.Error(), "delivery history forbids inbox selection") {
			t.Fatalf("storage fence did not reject inbox selection after a native reservation: %v", err)
		}
	})
	t.Run("probe-only answer without native history", func(t *testing.T) {
		s, in := actorDeliveryFixture(t)
		_, err := s.RecordSwarmRequestDelivery(ctx, in.SwarmID, in.SourceID, "unconfirmed")
		mustNil(t, err)
		intent, err := s.PrepareActorDelivery(ctx, in, allow)
		mustNil(t, err)
		if !intent.ProbeOnly {
			t.Fatal("fixture should retain legacy uncertainty")
		}
		if err := s.SelectActorDeliveryInbox(ctx, actorDeliveryInboxTarget(intent), allow); !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatalf("probe-only uncertainty opened a new delivery path: %v", err)
		}
	})
	t.Run("uncertainty recorded after preparation", func(t *testing.T) {
		s, target := actorDeliveryInboxFixture(t)
		_, err := s.RecordSwarmRequestDelivery(ctx, target.SwarmID, target.SourceID, "unconfirmed")
		mustNil(t, err)
		if err := s.SelectActorDeliveryInbox(ctx, target, allow); !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatalf("non-probe-only intent hid new source uncertainty: %v", err)
		}
	})
}

func TestActorDeliveryInboxSelectionAndNativeReservationCannotBothWin(t *testing.T) {
	ctx := context.Background()
	s, target := actorDeliveryInboxFixture(t)
	start := make(chan struct{})
	selected, reserved := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		selected <- s.SelectActorDeliveryInbox(ctx, target, allow)
	}()
	go func() {
		<-start
		now := time.Now().UTC()
		_, err := s.ReserveActorDeliveryAttempt(ctx, target.DeliveryID, now, now.Add(time.Minute), allow)
		reserved <- err
	}()
	close(start)
	selectionErr, reservationErr := <-selected, <-reserved
	if !(selectionErr == nil && errors.Is(reservationErr, ErrActorDeliveryConflict) || reservationErr == nil && errors.Is(selectionErr, ErrActorDeliveryConflict)) {
		t.Fatalf("expected exactly one delivery path: selection %v, native reservation %v", selectionErr, reservationErr)
	}
}

func TestActorDeliveryInboxCustodyDoesNotConfirmNativeDelivery(t *testing.T) {
	ctx := context.Background()
	s, target := actorDeliveryInboxFixture(t)
	record := func(target ActorDeliveryInboxTarget, disposition string, authorize func(context.Context) bool) error {
		return s.RecordActorDeliveryInboxCustody(ctx, target, disposition, authorize)
	}
	if err := record(target, inboxPositiveCustody, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("custody without receiver selection: %v", err)
	}
	mustNil(t, s.SelectActorDeliveryInbox(ctx, target, allow))
	for _, disposition := range []string{"", "receipt_absent", "receipt_unavailable", "delivered", "duplicate"} {
		if err := record(target, disposition, allow); !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatalf("recorded %q as positive custody: %v", disposition, err)
		}
	}
	for field, change := range map[string]func(*ActorDeliveryInboxTarget){
		"source":      func(v *ActorDeliveryInboxTarget) { v.SourceID = "other" },
		"actor":       func(v *ActorDeliveryInboxTarget) { v.ActorUID = "other" },
		"incarnation": func(v *ActorDeliveryInboxTarget) { v.Incarnation++ },
		"receiver":    func(v *ActorDeliveryInboxTarget) { v.ReceiverID = strings.Repeat("c", 64) },
		"digest":      func(v *ActorDeliveryInboxTarget) { v.Digest = strings.Repeat("d", 64) },
	} {
		wrong := target
		change(&wrong)
		if err := record(wrong, inboxPositiveCustody, allow); !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatalf("positive custody with changed %s: %v", field, err)
		}
	}
	if err := record(target, inboxPositiveCustody, func(context.Context) bool { return false }); !errors.Is(err, ErrActorDeliveryDenied) {
		t.Fatalf("custody without current authority: %v", err)
	}
	before, err := s.GetSwarmRequest(ctx, target.SwarmID, target.SourceID)
	mustNil(t, err)
	mustNil(t, record(target, inboxPositiveCustody, allow))
	mustNil(t, record(target, inboxPositiveCustody, allow))
	if err := s.ReconcileActorDelivery(ctx, target.DeliveryID, allow); !errors.Is(err, ErrActorDeliveryReceiptPending) {
		t.Fatalf("supervisor custody satisfied native reconciliation: %v", err)
	}
	history, err := s.GetActorDeliveryHistory(ctx, target.DeliveryID)
	mustNil(t, err)
	got, err := s.GetSwarmRequest(ctx, target.SwarmID, target.SourceID)
	mustNil(t, err)
	if len(history) != 0 || got.Delivery != before.Delivery || got.State != before.State {
		t.Fatalf("custody changed native history or source: history=%+v, request=%+v", history, got)
	}
}

func TestActorDeliveryInboxDispatchPermitCannotBeReissued(t *testing.T) {
	ctx := context.Background()
	s, target := actorDeliveryInboxFixture(t)
	const launch = "root-launch-1"
	mustNil(t, s.SelectActorDeliveryInbox(ctx, target, allow))
	authorized := func(txctx context.Context, exact ActorDeliveryInboxTarget, launchID string) bool {
		if exact != target || launchID != launch {
			return false
		}
		var mode int
		return s.conn(txctx).QueryRowContext(txctx, `PRAGMA synchronous`).Scan(&mode) == nil && mode == 2
	}
	reserve := func(scope ActorDeliveryInboxTarget, launchID string, authorize func(context.Context, ActorDeliveryInboxTarget, string) bool) (string, error) {
		return s.ReserveActorDeliveryInboxDispatch(ctx, scope, launchID, authorize)
	}
	if id, err := reserve(target, launch, authorized); id != "" || !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("permit without custody: %q, %v", id, err)
	}
	mustNil(t, s.RecordActorDeliveryInboxCustody(ctx, target, inboxPositiveCustody, allow))
	for _, invalid := range []string{"", "launch elsewhere", "../launch", strings.Repeat("x", 129), "launch\x00tail"} {
		if id, err := reserve(target, invalid, authorized); id != "" || !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatalf("permit without a valid launch ID: %q, %v", id, err)
		}
	}
	if id, err := reserve(target, launch, nil); id != "" || !errors.Is(err, ErrActorDeliveryDenied) {
		t.Fatalf("permit without launch authority: %q, %v", id, err)
	}
	if id, err := reserve(target, "late-launch", authorized); id != "" || !errors.Is(err, ErrActorDeliveryDenied) {
		t.Fatalf("permit for expired or unowned launch: %q, %v", id, err)
	}
	wrong := target
	wrong.ReceiverID = strings.Repeat("e", 64)
	authAny := func(context.Context, ActorDeliveryInboxTarget, string) bool { return true }
	if id, err := reserve(wrong, launch, authAny); id != "" || !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("permit for wrong scope: %q, %v", id, err)
	}
	_, err := s.db.Exec(`UPDATE swarms SET atespace_uid='replacement' WHERE id=?`, target.SwarmID)
	mustNil(t, err)
	if id, err := reserve(target, launch, authorized); id != "" || !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("permit for stale source: %q, %v", id, err)
	}
	_, err = s.db.Exec(`UPDATE swarms SET atespace_uid='native-s' WHERE id=?`, target.SwarmID)
	mustNil(t, err)

	permitID, err := reserve(target, launch, authorized)
	mustNil(t, err)
	if permitID == "" {
		t.Fatal("first committed reservation did not grant a permit")
	}
	var seq int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	// The returning guest can present the same valid custody again (or a new
	// launch ID). Neither may recover the original effect permission.
	if err := s.SelectActorDeliveryInbox(ctx, wrong, allow); !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("selection forgot the receiver after Store reopen: %v", err)
	}
	if id, err := reserve(target, launch, authorized); id != "" || !errors.Is(err, ErrActorDeliveryConflict) {
		t.Fatalf("recovered the first permit before refreshing custody: %q, %v", id, err)
	}
	mustNil(t, s.SelectActorDeliveryInbox(ctx, target, allow))
	mustNil(t, s.RecordActorDeliveryInboxCustody(ctx, target, inboxPositiveCustody, allow))
	for _, launchID := range []string{launch, "root-launch-2"} {
		if id, err := reserve(target, launchID, authAny); id != "" || !errors.Is(err, ErrActorDeliveryConflict) {
			t.Fatalf("recovered permit after Store reopen: %q, %v", id, err)
		}
	}
}

func TestActorDeliveryInboxPermitIsBoundAndAppendOnly(t *testing.T) {
	ctx := context.Background()
	s, target := actorDeliveryInboxFixture(t)
	mustNil(t, s.SelectActorDeliveryInbox(ctx, target, allow))
	mustNil(t, s.RecordActorDeliveryInboxCustody(ctx, target, inboxPositiveCustody, allow))
	type result struct {
		launch, id string
		err        error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for _, launch := range []string{"launch-one", "launch-two"} {
		go func() {
			<-start
			id, err := s.ReserveActorDeliveryInboxDispatch(ctx, target, launch, func(_ context.Context, got ActorDeliveryInboxTarget, gotLaunch string) bool {
				return got == target && gotLaunch == launch
			})
			results <- result{launch, id, err}
		}()
	}
	close(start)
	var winner result
	for range 2 {
		r := <-results
		if r.err == nil {
			if winner.id != "" || r.id == "" {
				t.Fatalf("unexpected competing permit: %+v, first %+v", r, winner)
			}
			winner = r
		} else if r.id != "" || !errors.Is(r.err, ErrActorDeliveryConflict) {
			t.Fatalf("losing launch did not fail closed: %+v", r)
		}
	}
	var storedID, storedLaunch string
	mustNil(t, s.db.QueryRow(`SELECT permit_id,launch_id FROM actor_delivery_inbox_permits WHERE delivery_id=?`, target.DeliveryID).Scan(&storedID, &storedLaunch))
	if winner.id == "" || winner.id != storedID || winner.launch != storedLaunch {
		t.Fatalf("returned permit did not match the committed launch: %+v", winner)
	}
	before := databaseRows(t, s.db)
	for table, field := range map[string]string{
		"actor_delivery_inbox_selections": "selected_at",
		"actor_delivery_inbox_custody":    "recorded_at",
		"actor_delivery_inbox_permits":    "launch_id",
	} {
		if len(before[table]) != 1 {
			t.Fatalf("expected one retained row in %s, got %v", table, before[table])
		}
		for _, statement := range []string{
			`UPDATE ` + table + ` SET ` + field + `='replacement'`,
			`DELETE FROM ` + table,
			`INSERT OR REPLACE INTO ` + table + ` SELECT * FROM ` + table,
		} {
			if _, err := s.db.Exec(statement); err == nil {
				t.Fatalf("storage rewrote retained evidence: %s", statement)
			}
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("rejected SQL mutations changed retained evidence")
	}
}

func TestActorDeliveryInboxMigrationPreservesVersion63Evidence(t *testing.T) {
	path := loadSchemaFixture(t, 63)
	old := rawDatabase(t, path)
	before := databaseRows(t, old)
	mustNil(t, old.Close())
	if len(before["actor_delivery_attempts"]) == 0 || len(before["actor_delivery_outcomes"]) == 0 {
		t.Fatal("version 63 fixture did not exercise retained native evidence")
	}
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != 64 {
		t.Fatalf("migration reached version %d, want 64", version)
	}
	if after, want := databaseRows(t, s.db), withEmptyActorDeliveryInboxTables(before); !reflect.DeepEqual(want, after) {
		t.Fatal("inbox migration changed pre-existing rows or did not add empty inbox tables")
	}
	requireIntact(t, s.db)
}

func withEmptyActorDeliveryInboxTables(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	for _, table := range []string{"actor_delivery_inbox_selections", "actor_delivery_inbox_custody", "actor_delivery_inbox_permits"} {
		if _, exists := out[table]; !exists {
			out[table] = nil
		}
	}
	return out
}
