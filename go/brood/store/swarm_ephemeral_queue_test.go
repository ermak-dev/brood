package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Очередь #292 хранит намерение до выделения актора. Полный пул не теряет
// дочернюю работу; обычная и эфемерная очередь делят один bounded join.
func TestEphemeralChildQueueRetainsFiveRequestsWithoutActorCapacity(t *testing.T) {
	ctx := context.Background()
	s, p, _ := childFixture(t, "all", 5, 0)
	pool, err := s.GetManagedPool(ctx, "pool")
	mustNil(t, err)
	actors, err := s.ListManagedActors(ctx, "")
	mustNil(t, err)
	if int64(len(actors)) != pool.MaxActors {
		t.Fatal("queue test does not actually exhaust pool actor capacity")
	}
	var saved []EphemeralChildIntent
	for index := 0; index < 5; index++ {
		intent, err := s.EnqueueEphemeralChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 5, Remaining: "retain"}, fmt.Sprintf("ephemeral-%d", index), fmt.Sprintf("Task %d", index), "pool", childAllow)
		mustNil(t, err)
		if intent.Parent != p || intent.PoolUID != "pool" || intent.SourceCommit != startCommit || intent.Origin != startOrigin || intent.Template != pool.Template || intent.SourceFingerprint != pool.SourceFingerprint || intent.QueuePosition < 1 {
			t.Fatalf("queue lost immutable task source/pool scope: %+v", intent)
		}
		saved = append(saved, intent)
	}
	after, err := s.ListManagedActors(ctx, "")
	mustNil(t, err)
	if !reflect.DeepEqual(actors, after) {
		t.Fatal("enqueue allocated or changed actors before capacity admission")
	}
	queued, err := s.QueuedEphemeralChildIntents(ctx, 0)
	mustNil(t, err)
	if !reflect.DeepEqual(saved, queued) {
		t.Fatal("queue lost FIFO insertion order or retained source material")
	}
	tail, err := s.QueuedEphemeralChildIntents(ctx, saved[2].QueuePosition)
	mustNil(t, err)
	if !reflect.DeepEqual(saved[3:], tail) {
		t.Fatal("queue cursor skipped or repeated pending work")
	}
	join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if len(join.Children) != 0 || join.Evaluation.Open != 5 || join.State != "open" {
		t.Fatal("unallocated queue falsely completed membership or released join")
	}
	before := databaseRows(t, s.db)
	_, err = s.EnqueueEphemeralChild(ctx, p, join.Policy, "overflow", "Too much work", "pool", childAllow)
	if !errors.Is(err, ErrSwarmChildConflict) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("queue overflow returned success or partial state")
	}
}

func TestEphemeralChildQueueExactRetryIsReadOnlyAndVariantsConflict(t *testing.T) {
	ctx := context.Background()
	s, p, _ := childFixture(t, "all", 2, 0)
	policy := SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}
	original, err := s.EnqueueEphemeralChild(ctx, p, policy, "stable", "Exact payload", "pool", childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	repeated, err := s.EnqueueEphemeralChild(ctx, p, policy, "stable", "Exact payload", "pool", childAllow)
	mustNil(t, err)
	if !reflect.DeepEqual(original, repeated) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("identical queue retry changed identity, FIFO position or history")
	}
	read, err := s.GetEphemeralChildIntent(ctx, original.ID)
	mustNil(t, err)
	if !reflect.DeepEqual(original, read) {
		t.Fatal("historical queue read changed stored intent")
	}
	_, err = s.EnqueueEphemeralChild(ctx, p, policy, "stable", "Changed payload", "pool", childAllow)
	if !errors.Is(err, ErrSwarmChildConflict) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("changed retry replaced intent or created another queued job")
	}
}

func TestEphemeralAndRoleChildrenCannotStealEachOthersKeysOrSlots(t *testing.T) {
	ctx := context.Background()
	s, p, executor := childFixture(t, "all", 2, 0)
	policy := SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}
	_, err := s.EnqueueEphemeralChild(ctx, p, policy, "reserved", "Ephemeral payload", "pool", childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	_, err = s.ReserveSwarmChild(ctx, p, "reserved", "Ephemeral payload", executor, childAllow)
	if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ordinary child stole an unallocated ephemeral key")
	}
	reserveChild(t, s, p, executor, "ordinary")
	before = databaseRows(t, s.db)
	_, err = s.EnqueueEphemeralChild(ctx, p, policy, "ordinary", "Implement the parser test.", "pool", childAllow)
	if !errors.Is(err, ErrSwarmChildConflict) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ephemeral queue stole an existing ordinary child")
	}
	_, err = s.ReserveSwarmChild(ctx, p, "overflow", "Third slot", executor, childAllow)
	if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ordinary children ignored an occupied ephemeral slot")
	}
}

func TestEphemeralChildQueueRechecksParentAuthorityAndRollsBackCaughtFailure(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	attestOne(t, s, "parent")
	op, err := s.CurrentActorOperation(ctx, "a")
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	p := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "one", SwarmID: "one", AtespaceUID: "native-one", ActorUID: "a", Incarnation: 1}, Pass: 1, Sequence: 1, RequestID: "parent"}
	policy := SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}
	before := databaseRows(t, s.db)
	err = s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.EnqueueEphemeralChild(ctx, p, policy, "invalid-pool", "Payload", "missing", childAllow); err == nil {
			return errors.New("unknown pool was admitted")
		}
		return nil
	})
	mustNil(t, err)
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("caught admission failure committed a new join, queue row or notification")
	}
	if _, err := s.EnqueueEphemeralChild(ctx, p, policy, "denied", "Payload", "pool", nil); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatal("nil authorization admitted ephemeral work")
	}
	stale := p
	stale.Principal.Incarnation++
	if _, err := s.EnqueueEphemeralChild(ctx, stale, policy, "stale", "Payload", "pool", childAllow); err == nil {
		t.Fatal("stale authenticated parent admitted queued work")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("refused parent admission changed source state")
	}
}

func TestEphemeralChildQueueHistoryAndScopeCannotBeRewritten(t *testing.T) {
	ctx := context.Background()
	s, p, _ := childFixture(t, "all", 2, 0)
	saved, err := s.EnqueueEphemeralChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "immutable", "Payload", "pool", childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	for _, query := range []string{
		`UPDATE ephemeral_child_intents SET payload='rewritten' WHERE id=?`,
		`DELETE FROM ephemeral_child_intents WHERE id=?`,
		`INSERT OR REPLACE INTO ephemeral_child_intents SELECT * FROM ephemeral_child_intents WHERE id=?`,
	} {
		if _, err := s.db.Exec(query, saved.ID); err == nil {
			t.Fatal("queued intent history was rewritten")
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("immutable history refusal changed source rows")
		}
	}
	for _, field := range []string{"project_id", "atespace_uid", "parent_uid", "parent_request_id"} {
		columns := strings.Split(ephemeralChildIntentCols, ",")
		expressions := append([]string(nil), columns...)
		for index, column := range columns {
			if column == "id" || column == "request_id" || column == field {
				expressions[index] = "'forged-scope'"
			}
		}
		query := `INSERT INTO ephemeral_child_intents (` + ephemeralChildIntentCols + `) SELECT ` + strings.Join(expressions, ",") + ` FROM ephemeral_child_intents WHERE id=?`
		if _, err := s.db.Exec(query, saved.ID); err == nil {
			t.Fatal("queue slot accepted foreign parent scope")
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("scope refusal changed queue or execution state")
		}
	}
}

func TestEphemeralChildQueueConcurrentAdmissionsRespectTotal(t *testing.T) {
	s, p, _ := childFixture(t, "all", 3, 0)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := s.EnqueueEphemeralChild(context.Background(), p, SwarmChildJoinPolicy{Mode: "all", Total: 3, Remaining: "retain"}, fmt.Sprintf("task-%d", index), "Payload", "pool", childAllow)
			results <- err
		}(index)
	}
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrSwarmChildConflict) {
			t.Fatal(err)
		}
	}
	intents, err := s.QueuedEphemeralChildIntents(context.Background(), 0)
	mustNil(t, err)
	if admitted != 3 || len(intents) != 3 {
		t.Fatal("concurrent enqueues overbooked or lost join slots")
	}
}
