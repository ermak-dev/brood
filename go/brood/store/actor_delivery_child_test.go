package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// FEATURE: Одна durable инструкция для proof-bound ephemeral child (#292).
// SPEC: Атомарный FULL reserve/start, current authority, неизменный retry, без legacy dispatch.
// IMPLEMENTATION: Typed child intent и start CAS в одной транзакции, общий current scope.
// RESULT: IMPLEMENTED — до реализации отсутствовал API и ordinary start обходил ledger.
// STATUS: DONE
func prepareChildDelivery(t *testing.T, s *Store, ctx context.Context, id string, in ActorDeliveryInput, auth SwarmChildAuthorize) (ActorDeliveryIntent, error) {
	t.Helper()
	return s.PrepareEphemeralChildDelivery(ctx, id, in, auth)
}

func childDeliveryFixture(t *testing.T) (*Store, EphemeralChildIntent, SwarmChildTask, ActorDeliveryInput) {
	t.Helper()
	s, intent := ephemeralAttestationFixture(t, true)
	attestForRegistration(t, s, intent)
	child, err := s.RegisterEphemeralChild(context.Background(), intent.ID, childAllow)
	mustNil(t, err)
	in := ActorDeliveryInput{SwarmID: child.Executor.SwarmID, SourceKind: "child", SourceID: child.ID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: child.Executor.ActorUID, Incarnation: child.Executor.Incarnation, AtespaceUID: child.Executor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"to":"isolated","body":"exact instruction\n","waitSec":0}`}
	return s, intent, child, in
}

func TestChildDeliveryAtomicFullStartAndRetry(t *testing.T) {
	ctx := context.Background()
	s, intent, child, in := childDeliveryFixture(t)
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 || a.Action != "prepare_ephemeral_delivery" || a.Parent != intent.Parent || a.Executor != child.Executor || a.Payload != intent.Payload || a.RequestID != intent.RequestID {
			t.Fatal("preparation lost durable authority scope")
		}
		return nil
	}
	delivery, err := prepareChildDelivery(t, s, ctx, intent.ID, in, auth)
	mustNil(t, err)
	if delivery.Input != in || delivery.ID == "" || delivery.ProbeOnly {
		t.Fatalf("wrong identity: %+v", delivery)
	}
	join, err := s.GetSwarmChildJoin(ctx, intent.Parent, childAllow)
	mustNil(t, err)
	running := join.Children[0]
	child.State, child.Revision = "running", 2
	if running != child {
		t.Fatalf("preparation must only start exact child: %+v", running)
	}
	before := databaseRows(t, s.db)
	retry, err := prepareChildDelivery(t, s, ctx, intent.ID, in, auth)
	mustNil(t, err)
	if retry != delivery || calls != 2 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retry changed history or skipped authority")
	}
	for _, scan := range []func(context.Context, string, int) ([]ActorDeliveryCandidate, error){s.PendingActorDeliveryAnswers, s.PendingActorDeliverySteps} {
		got, err := scan(ctx, "", 64)
		mustNil(t, err)
		if len(got) != 0 {
			t.Fatal("child activated legacy dispatcher")
		}
	}
	if _, err := s.PrepareActorDelivery(ctx, in, allow); err == nil {
		t.Fatal("generic preparation accepted child")
	}
}

func TestChildDeliveryChangedInputAndAuthorityRefuseWithoutWrites(t *testing.T) {
	for _, change := range []string{"nil", "denied", "swarm", "source", "kind", "request", "commit", "actor", "incarnation", "namespace", "journal", "payload", "intent"} {
		t.Run(change, func(t *testing.T) {
			s, intent, _, in := childDeliveryFixture(t)
			_, err := prepareChildDelivery(t, s, context.Background(), intent.ID, in, childAllow)
			mustNil(t, err)
			auth, id := SwarmChildAuthorize(childAllow), intent.ID
			switch change {
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "swarm":
				in.SwarmID = "other"
			case "source":
				in.SourceID = "other"
			case "kind":
				in.SourceKind = "step"
			case "request":
				in.RequestID = "other"
			case "commit":
				in.Commit = "other"
			case "actor":
				in.ActorUID = "a"
			case "incarnation":
				in.Incarnation++
			case "namespace":
				in.AtespaceUID = "other"
			case "journal":
				in.JournalID = strings.Repeat("b", 64)
			case "payload":
				in.Payload = `{"body":"changed"}`
			case "intent":
				id = "other"
			}
			before := databaseRows(t, s.db)
			got, err := prepareChildDelivery(t, s, context.Background(), id, in, auth)
			if err == nil || got != (ActorDeliveryIntent{}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("changed retry accepted or changed rows")
			}
		})
	}
}

func TestChildDeliveryAtomicityNestingAndConcurrency(t *testing.T) {
	ctx := context.Background()
	s, intent, _, in := childDeliveryFixture(t)
	before := databaseRows(t, s.db)
	for _, run := range []func(context.Context, func(context.Context) error) error{s.WithTx, s.WithDurableTx} {
		mustNil(t, run(ctx, func(ctx context.Context) error {
			_, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow)
			if !errors.Is(err, ErrDurableTransactionNested) {
				t.Fatal("nested preparation accepted")
			}
			return nil
		}))
	}
	canceled, cancel := context.WithCancel(ctx)
	_, err := prepareChildDelivery(t, s, canceled, intent.ID, in, func(context.Context, SwarmChildAuthorization) error { cancel(); return nil })
	if err == nil {
		t.Fatal("cancelled preparation accepted")
	}
	mustNil(t, s.WithTx(ctx, func(context.Context) error { return nil }))
	for _, table := range []string{"actor_deliveries", "swarm_child_tasks"} {
		event := "INSERT"
		if table == "swarm_child_tasks" {
			event = "UPDATE"
		}
		_, err = s.db.Exec(`CREATE TRIGGER fail_child_delivery AFTER ` + event + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected failure'); END`)
		mustNil(t, err)
		if _, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow); err == nil {
			t.Fatal("injected failure accepted")
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("partial reservation/start leaked")
		}
		_, err = s.db.Exec(`DROP TRIGGER fail_child_delivery`)
		mustNil(t, err)
	}
	var wg sync.WaitGroup
	results := make(chan ActorDeliveryIntent, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow)
			results <- got
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	var first ActorDeliveryIntent
	for got := range results {
		if first.ID == "" {
			first = got
		} else if first != got {
			t.Fatal("concurrent identity changed")
		}
	}
	var count, revision int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM actor_deliveries`).Scan(&count))
	mustNil(t, s.db.QueryRow(`SELECT revision FROM swarm_child_tasks WHERE id=?`, in.SourceID).Scan(&revision))
	if count != 1 || revision != 2 {
		t.Fatal("concurrent preparation repeated delivery/start")
	}
}

func TestChildDeliveryUnknownProbeAndKnownNotSent(t *testing.T) {
	for _, outcome := range []string{"", "unconfirmed", "not_sent"} {
		t.Run("outcome-"+outcome, func(t *testing.T) {
			ctx := context.Background()
			s, intent, _, in := childDeliveryFixture(t)
			d, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow)
			mustNil(t, err)
			now := time.Now().UTC()
			a, err := s.ReserveActorDeliveryAttempt(ctx, d.ID, now, now.Add(time.Second), allow)
			mustNil(t, err)
			if a.Mode != "send" {
				t.Fatal("first attempt not a send")
			}
			if outcome != "" {
				mustNil(t, s.RecordActorDeliveryOutcome(ctx, a, outcome, "evidence", now, allow))
			}
			a, err = s.ReserveActorDeliveryAttempt(ctx, d.ID, now.Add(time.Minute), now.Add(2*time.Minute), allow)
			mustNil(t, err)
			want, receipt := "probe", "duplicate"
			if outcome == "not_sent" {
				want, receipt = "send", "delivered"
			}
			if a.Mode != want {
				t.Fatalf("mode=%s want=%s", a.Mode, want)
			}
			mustNil(t, s.RecordActorDeliveryOutcome(ctx, a, receipt, "receipt", now.Add(time.Minute), allow))
			before := databaseRows(t, s.db)
			mustNil(t, s.ReconcileActorDelivery(ctx, d.ID, allow))
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("receipt completed child or advanced graph")
			}
			if _, err := s.ReserveActorDeliveryAttempt(ctx, d.ID, now.Add(3*time.Minute), now.Add(4*time.Minute), allow); !errors.Is(err, ErrActorDeliveryComplete) {
				t.Fatal("receipt allowed another attempt")
			}
		})
	}
}

func TestChildDeliveryRestartRetainsIdentityAndUncertainty(t *testing.T) {
	ctx := context.Background()
	s, intent, child, in := childDeliveryFixture(t)
	d, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow)
	mustNil(t, err)
	now := time.Now().UTC()
	_, err = s.ReserveActorDeliveryAttempt(ctx, d.ID, now, now.Add(time.Second), allow)
	mustNil(t, err)
	var sequence int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	s.Close()
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	before := databaseRows(t, reopened.db)
	retry, err := prepareChildDelivery(t, reopened, ctx, intent.ID, in, childAllow)
	mustNil(t, err)
	if retry != d || !reflect.DeepEqual(before, databaseRows(t, reopened.db)) {
		t.Fatal("restart rewrote original identity or child")
	}
	probe, err := reopened.ReserveActorDeliveryAttempt(ctx, d.ID, now.Add(time.Minute), now.Add(2*time.Minute), allow)
	mustNil(t, err)
	if probe.Mode != "probe" {
		t.Fatal("restart replayed unknown payload")
	}
	mustNil(t, reopened.RecordActorDeliveryOutcome(ctx, probe, "duplicate", "receipt", now.Add(time.Minute), allow))
	mustNil(t, reopened.CheckActorChildObligations(ctx, "unrelated-actor"))
	if err := reopened.CheckActorChildObligations(ctx, child.Executor.ActorUID); err == nil {
		t.Fatal("receipt released child hold")
	}
	if _, err := reopened.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, 2, "success", "independent report", childAllow); err != nil {
		t.Fatal(err)
	}
}

func TestChildDeliveryOrdinaryChildCannotGainInstruction(t *testing.T) {
	ctx := context.Background()
	s, intent := registrationFixture(t, 2)
	child, err := s.RegisterEphemeralChild(ctx, intent.ID, childAllow)
	mustNil(t, err)
	in := ActorDeliveryInput{SwarmID: child.Executor.SwarmID, SourceKind: "child", SourceID: child.ID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: child.Executor.ActorUID, Incarnation: child.Executor.Incarnation, AtespaceUID: child.Executor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"body":"instruction"}`}
	executor := intent.Parent.Principal
	executor.ActorUID = "b"
	ordinary, err := s.ReserveSwarmChild(ctx, intent.Parent, "ordinary", "ad hoc", executor, childAllow)
	mustNil(t, err)
	in.SourceID, in.ActorUID, in.RequestID = ordinary.ID, ordinary.Executor.ActorUID, ordinary.RequestID
	before := databaseRows(t, s.db)
	if _, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow); err == nil {
		t.Fatal("ordinary child gained proof-bound delivery")
	}
	if _, err := s.PrepareActorDelivery(ctx, in, allow); err == nil {
		t.Fatal("generic preparation bypassed child scope")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ordinary preparation changed state")
	}
}

func TestChildDeliveryOrdinaryStartCannotBypass(t *testing.T) {
	s, intent, child, _ := childDeliveryFixture(t)
	before := databaseRows(t, s.db)
	if _, err := s.StartSwarmChild(context.Background(), intent.Parent, child.RequestID, child.Executor, child.Revision, childAllow); err == nil {
		t.Fatal("ordinary start bypassed durable instruction reservation")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ordinary start changed child")
	}
}
