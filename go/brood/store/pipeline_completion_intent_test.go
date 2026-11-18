package store

// FEATURE: Completion intent сохраняется до result observation (#290).
// SPEC: Исход/отчёт immutable; точный повтор идемпотентен; граф и actual outcome
// не продвигаются; старый completion не обходит pending intent.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func completionIntentFixture(t *testing.T) (*Store, ActorCompletion) {
	t.Helper()
	s := dispatchStore(t)
	attempt := deliverOne(t, s, "result-request")
	return s, ActorCompletion{SwarmID: "one", Sequence: attempt.Sequence, RequestID: attempt.RequestID, ActorUID: attempt.Actor.UID, Incarnation: attempt.Actor.Incarnation, Outcome: "success", Report: "Implemented candidate; result observation pending."}
}

func TestCompletionIntentPreservesOutcomeWithoutAdvancingGraph(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	before := dumpTables(t, s, "swarms", "swarm_steps", "swarm_requests", "telegram_outbox")
	first, err := s.RecordPipelineCompletionIntent(ctx, in, allow)
	mustNil(t, err)
	if first.Pass != 1 || first.BaseCommit != startCommit || first.CreatedAt == "" || first.ActorCompletion != in {
		t.Fatalf("incomplete intent: %+v", first)
	}
	again, err := s.RecordPipelineCompletionIntent(ctx, in, allow)
	mustNil(t, err)
	if !reflect.DeepEqual(first, again) {
		t.Fatal("exact retry changed intent")
	}
	if dumpTables(t, s, "swarms", "swarm_steps", "swarm_requests", "telegram_outbox") != before {
		t.Fatal("intent advanced graph or rewrote actual outcome")
	}
	if err := completeWithReport(s, ActorStepAttempt{Sequence: in.Sequence, RequestID: in.RequestID, Actor: ManagedActor{UID: in.ActorUID, Incarnation: in.Incarnation}}, in.Outcome, in.Report); !errors.Is(err, ErrPipelineCompletionPending) {
		t.Fatalf("old completion bypassed pending intent: %v", err)
	}
	if stepCount(t, s) != 1 || stepOf(t, s, 1).Outcome != nil {
		t.Fatal("pending intent created a successor or fabricated completion")
	}
}

func TestCompletionIntentRejectsChangedPayloadAndCurrentAuthority(t *testing.T) {
	for _, kind := range []string{"outcome", "report", "actor", "incarnation", "attempt", "session"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, in := completionIntentFixture(t)
			_, err := s.RecordPipelineCompletionIntent(ctx, in, allow)
			mustNil(t, err)
			before := dumpTables(t, s, "pipeline_completion_intents")
			authorize := allow
			switch kind {
			case "outcome":
				in.Outcome = "fail"
			case "report":
				in.Report = "changed"
			case "actor":
				in.ActorUID = "b"
			case "incarnation":
				in.Incarnation++
			case "attempt":
				in.RequestID = "forged"
			case "session":
				authorize = func(context.Context) bool { return false }
			}
			if _, err := s.RecordPipelineCompletionIntent(ctx, in, authorize); err == nil {
				t.Fatal("changed completion accepted")
			}
			if dumpTables(t, s, "pipeline_completion_intents") != before {
				t.Fatal("refused retry changed retained intent")
			}
		})
	}
}

func TestCompletionIntentSurvivesOwnerStopAndCannotBeRewritten(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	saved, err := s.RecordPipelineCompletionIntent(ctx, in, allow)
	mustNil(t, err)
	stopOne(t, s)
	if _, err := s.RecordPipelineCompletionIntent(ctx, in, allow); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("late completion reopened stopped pass: %v", err)
	}
	current, err := s.GetPipelineCompletionIntent(ctx, "one", 1)
	mustNil(t, err)
	if !reflect.DeepEqual(current, saved) {
		t.Fatal("owner stop erased reported intent")
	}
	for _, query := range []string{
		`UPDATE pipeline_completion_intents SET outcome='fail'`,
		`DELETE FROM pipeline_completion_intents`,
		`INSERT OR REPLACE INTO pipeline_completion_intents SELECT * FROM pipeline_completion_intents`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "pipeline completion intent is immutable") {
			t.Fatalf("intent history fence: %v", err)
		}
	}
}

func TestCompletionIntentSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	s, in := completionIntentFixture(t)
	intent, err := s.RecordPipelineCompletionIntent(ctx, in, allow)
	mustNil(t, err)
	var sequence int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	got, err := reopened.GetPipelineCompletionIntent(ctx, in.SwarmID, in.Sequence)
	mustNil(t, err)
	if !reflect.DeepEqual(got, intent) || stepCount(t, reopened) != 1 || stepOf(t, reopened, 1).State != "inflight" {
		t.Fatal("reopen lost intent or advanced an unverified result")
	}
}

func TestPolicyActorCompletionCannotBypassIntentAcceptance(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [work],", 1)
	s := policyPreviewStore(t, graph, policyRules())
	attempt := deliverOne(t, s, "policy-direct")
	if err := completeWithReport(s, attempt, "success", "direct completion"); !errors.Is(err, ErrPipelineCompletionPending) {
		t.Fatalf("direct store completion bypassed policy acceptance: %v", err)
	}
	step, err := s.GetSwarmStep(ctx, "one", attempt.Sequence)
	mustNil(t, err)
	if step.Outcome != nil || step.State == "completed" {
		t.Fatal("direct store completion advanced protected pass")
	}
}

func TestCompletionIntentRequiresVerifiedInputAndValidReport(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	attempt := reserveOne(t, s, "unattested")
	in := ActorCompletion{SwarmID: "one", Sequence: 1, RequestID: attempt.RequestID, ActorUID: "a", Incarnation: 1, Outcome: "success"}
	if _, err := s.RecordPipelineCompletionIntent(ctx, in, allow); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("unverified input accepted: %v", err)
	}
	in.Report = "bad\x00report"
	if _, err := s.RecordPipelineCompletionIntent(ctx, in, allow); !errors.Is(err, ErrStepReportInvalid) {
		t.Fatalf("invalid report accepted: %v", err)
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM pipeline_completion_intents`).Scan(&count))
	if count != 0 {
		t.Fatal("refused input persisted")
	}
}
