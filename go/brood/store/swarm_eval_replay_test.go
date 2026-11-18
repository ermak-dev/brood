package store

// FEATURE: Durable eval replay без подмены reference human (#293).
// SPEC: Только точный waiting node/occurrence и полный набор options;
// собственная provenance с исходным request ID/RecordedBy, answered_by launcher.
// Mismatch, actor и policy questions останавливают с retained reason, не ответом.
// FULL, nonnested, current authority on retries, atomic ignored-write refusal.
// IMPLEMENTATION: Pure matcher и graph transition в одной FULL-транзакции с readback.
// RESULT: IMPLEMENTED — normal/race и полный Store suite зелёные.
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func evalWaiting(t *testing.T, graph string) (*Store, SwarmEvalReplayInput) {
	t.Helper()
	ctx := context.Background()
	s, in := evalFixture(t)
	in.LauncherID = "root"
	publish(t, s, "two", map[string]string{"approve": graph})
	_, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	_, err = s.StartSwarmEval(ctx, "two", "root", evalAuthorize)
	mustNil(t, err)
	requests, err := s.SwarmRequests(ctx, "two")
	mustNil(t, err)
	if len(requests) != 1 {
		t.Fatalf("waiting requests: %+v", requests)
	}
	return s, SwarmEvalReplayInput{CandidateSwarmID: "two", RequestID: requests[0].ID, LauncherID: "root"}
}

func TestSwarmEvalReplayRecordsOwnProvenanceNotReferenceIdentity(t *testing.T) {
	ctx := context.Background()
	s, in := evalWaiting(t, startApprove)
	original := openNodeRequestOf(t, s, 1)
	result, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	mustNil(t, err)
	if result.State != "applied" || result.Reason != "" || result.Answer == nil || result.Answer.RecordedBy != "u" || result.Answer.ReferenceRequestID != original.ID || result.Answer.Occurrence != 1 || result.Answer.NodeID != "ask" {
		t.Fatalf("provenance: %+v", result)
	}
	request, err := s.GetSwarmRequest(ctx, "two", in.RequestID)
	mustNil(t, err)
	if request.State != "answered" || request.AnsweredBy != "root" || request.Outcome == nil || *request.Outcome != "success" || request.Response == nil || *request.Response != "Original human notes" {
		t.Fatalf("impersonated or wrong answer: %+v", request)
	}
	swarm, err := s.GetSwarm(ctx, "two")
	mustNil(t, err)
	if swarm.Execution != "succeeded" {
		t.Fatalf("did not advance: %s", swarm.Execution)
	}
	before := databaseRows(t, s.db)
	retry, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	mustNil(t, err)
	if !reflect.DeepEqual(result, retry) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retry changed applied replay")
	}
	denied := func(context.Context, string, SwarmEvalBinding) error { return ErrSwarmRequestDenied }
	_, err = s.ApplySwarmEvalReplay(ctx, in, denied, nil)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("revoked retry: %v", err)
	}
	in.LauncherID = "u"
	_, err = s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("foreign launcher: %v", err)
	}
}

func TestSwarmEvalReplayMismatchDurablyStopsWithoutAnswer(t *testing.T) {
	ctx := context.Background()
	s, in := evalWaiting(t, strings.Replace(startApprove, "label: Approve", "label: Changed", 1))
	result, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	mustNil(t, err)
	if result.State != "stopped" || !strings.Contains(result.Reason, "options changed") || result.Answer != nil {
		t.Fatalf("mismatch not retained: %+v", result)
	}
	swarm, err := s.GetSwarm(ctx, "two")
	mustNil(t, err)
	req, err := s.GetSwarmRequest(ctx, "two", in.RequestID)
	mustNil(t, err)
	if swarm.Execution != "stopped" || req.State != "cancelled" || req.AnsweredBy != "" || req.Outcome != nil || req.Response != nil {
		t.Fatalf("mismatch fabricated answer: %+v %+v", swarm, req)
	}
	got, err := s.GetSwarmEvalReplay(ctx, "two", in.RequestID)
	mustNil(t, err)
	if !reflect.DeepEqual(got, result) {
		t.Fatal("durable reason not readable")
	}
	retry, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	mustNil(t, err)
	if !reflect.DeepEqual(result, retry) {
		t.Fatal("stop retry changed reason")
	}
}

func TestSwarmEvalReplayRequiresAuthorityAndFullNonNestedCommit(t *testing.T) {
	ctx := context.Background()
	s, in := evalWaiting(t, startApprove)
	_, err := s.ApplySwarmEvalReplay(ctx, in, nil, nil)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("nil authority: %v", err)
	}
	mustNil(t, s.WithTx(ctx, func(tx context.Context) error {
		_, err := s.ApplySwarmEvalReplay(tx, in, evalAuthorize, nil)
		if !errors.Is(err, ErrDurableTransactionNested) {
			t.Fatalf("nested replay: %v", err)
		}
		return nil
	}))
	auth := func(tx context.Context, action string, b SwarmEvalBinding) error {
		var full int
		mustNil(t, s.conn(tx).QueryRowContext(tx, `PRAGMA synchronous`).Scan(&full))
		if full != 2 || action != "replay_eval" {
			t.Fatalf("wrong transaction/action: %d %s", full, action)
		}
		return nil
	}
	_, err = s.ApplySwarmEvalReplay(ctx, in, auth, nil)
	mustNil(t, err)
}

func TestSwarmEvalReplayIgnoredWritesRollBackAnswerAndProgress(t *testing.T) {
	for _, variant := range []string{"ledger", "answer", "stop", "stop-request"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			graph := startApprove
			if variant == "stop" || variant == "stop-request" {
				graph = strings.Replace(graph, "label: Approve", "label: Changed", 1)
			}
			s, in := evalWaiting(t, graph)
			trigger := `CREATE TRIGGER ignore_replay BEFORE INSERT ON swarm_eval_replays BEGIN SELECT RAISE(IGNORE); END`
			if variant == "answer" {
				trigger = `CREATE TRIGGER ignore_replay BEFORE UPDATE ON swarm_requests WHEN NEW.swarm_id='two' BEGIN SELECT RAISE(IGNORE); END`
			}
			if variant == "stop" {
				trigger = `CREATE TRIGGER ignore_replay BEFORE UPDATE ON swarms WHEN NEW.id='two' BEGIN SELECT RAISE(IGNORE); END`
			}
			if variant == "stop-request" {
				trigger = `CREATE TRIGGER ignore_replay BEFORE UPDATE ON swarm_requests WHEN NEW.swarm_id='two' AND NEW.state='cancelled' BEGIN SELECT RAISE(IGNORE); END`
			}
			_, err := s.db.Exec(trigger)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			_, err = s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
			if err == nil {
				t.Fatal("ignored write reported success")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("failed replay left partial state")
			}
		})
	}
}
