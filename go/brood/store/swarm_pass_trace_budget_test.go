package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPassTraceRefusesOversizedVisitSetWithoutPartialMaterial(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		for sequence := 2; sequence <= 4097; sequence++ {
			if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES('one',?,'build','pending')`, sequence); err != nil {
				return err
			}
		}
		return nil
	}))
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	if !errors.Is(err, ErrPassTraceLimit) || !reflect.DeepEqual(trace, SwarmPassTrace{}) {
		t.Fatalf("unbounded pass returned complete or partial material: visits=%d error=%v", len(trace.Steps), err)
	}
}

func TestPassTraceBoundsUnscopedQuestionsWithoutReturningPartialHistory(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		for n := 0; n < MaxPassTraceRecords; n++ {
			_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,options,state,created_at) VALUES(?,'one','actor','a',1,'text','Legacy unscoped question.','[]','open','2040-01-01T00:00:00Z')`, fmt.Sprintf("unscoped-%d", n))
			if err != nil {
				return err
			}
		}
		return nil
	}))
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	if !errors.Is(err, ErrPassTraceLimit) || !reflect.DeepEqual(trace, SwarmPassTrace{}) {
		t.Fatal("trace materialized an unbounded or truncated unscoped question collection")
	}
}

func TestPassTraceDoesNotChargeOtherPassQuestionsAgainstSelectedPassBudget(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	mustStart(t, s, "release")
	original := openNodeRequestOf(t, s, 1)
	stopOne(t, s)
	mustStart(t, s, "release")
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		for n := 0; n <= MaxPassTraceRecords; n++ {
			id := fmt.Sprintf("other-pass-%d", n)
			_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,options,state,created_at) VALUES(?,'one','actor','a',1,'text','Question in another pass.','[]','open','2040-01-01T00:00:00Z')`, id)
			if err != nil {
				return err
			}
			if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_request_pass_contexts VALUES(?,'one',2,2)`, id); err != nil {
				return err
			}
		}
		return nil
	}))
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	if len(trace.Steps) != 1 || len(trace.Requests) != 1 || trace.Requests[0].Request.ID != original.ID || len(trace.UnscopedActorRequests) != 0 {
		t.Fatal("selected-pass budget scanned or exposed another pass's question payloads")
	}
}

func TestPassTraceRefusesOversizedMaterialWithoutTruncatingFrames(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	payload := `{"body":"` + strings.Repeat("a", 128*1024-32) + `"}`
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		for sequence := 1; sequence <= 260; sequence++ {
			if sequence > 1 {
				if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES('one',?,'build','pending')`, sequence); err != nil {
					return err
				}
			}
			_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_deliveries VALUES(?,'one','step',?,?,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','a',1,'native-one',?,?,0,'recorded')`, fmt.Sprintf("trace-frame-%d", sequence), strconv.Itoa(sequence), fmt.Sprintf("trace-request-%d", sequence), strings.Repeat("a", 64), payload)
			if err != nil {
				return err
			}
		}
		return nil
	}))
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	if !errors.Is(err, ErrPassTraceLimit) || !reflect.DeepEqual(trace, SwarmPassTrace{}) {
		t.Fatalf("oversized instruction material returned or silently truncated: visits=%d error=%v", len(trace.Steps), err)
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM actor_deliveries WHERE swarm_id='one'`).Scan(&count))
	if count != 260 {
		t.Fatal("read budget mutated retained instruction history")
	}
}
