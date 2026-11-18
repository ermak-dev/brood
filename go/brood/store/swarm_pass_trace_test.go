package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/attest"
)

func TestPassTraceIsolatesPassesAndLabelsActorQuestionContext(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	unscoped := passContextQuestion(t, s)
	_, err := startWithGoal(s, "release", "First trace goal")
	mustNil(t, err)
	firstNode := openNodeRequestOf(t, s, 1)
	firstQuestion := passContextQuestion(t, s)
	answerNode(t, s, 1, "success")
	stopOne(t, s)
	_, err = startWithGoal(s, "release", "Second trace goal")
	mustNil(t, err)
	secondQuestion := passContextQuestion(t, s)
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: firstQuestion.ID, UserID: "u", Response: "A late answer still belongs to the original question."}, allow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	if trace.Version != 1 || trace.SwarmID != "one" || trace.Pass != 1 || trace.Goal != "First trace goal" || trace.SourceCommit != startCommit || trace.Current || trace.CurrentExecution != "" || len(trace.Graph) == 0 {
		t.Fatalf("trace inherited mutable current-pass metadata: %+v", trace)
	}
	if len(trace.Steps) != 2 || trace.Steps[0].Visit.Sequence != 1 || trace.Steps[1].Visit.Sequence != 2 || trace.Steps[1].Visit.Pass != 1 {
		t.Fatalf("trace crossed pass visit boundaries: %+v", trace.Steps)
	}
	ids := map[string]bool{}
	for _, item := range trace.Requests {
		ids[item.Request.ID] = true
		if item.Request.ID == firstQuestion.ID {
			if item.CreatedDuring == nil || item.CreatedDuring.Pass != 1 || item.CreatedDuring.ObservedSequence != 1 || item.Request.Sequence != nil || item.Request.State != "answered" || item.Request.Response == nil || *item.Request.Response != "A late answer still belongs to the original question." {
				t.Fatal("trace confused contextual creation with actor ownership of a graph step")
			}
		}
	}
	if !ids[firstNode.ID] || !ids[firstQuestion.ID] || ids[secondQuestion.ID] || ids[unscoped.ID] || len(ids) != 2 {
		t.Fatalf("trace leaked another pass or assigned an unscoped question: %+v", ids)
	}
	if len(trace.UnscopedActorRequests) != 1 || trace.UnscopedActorRequests[0].ID != unscoped.ID {
		t.Fatal("trace silently lost or falsely attributed unscoped actor history")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("reading a pass trace wrote source, notification or execution state")
	}
	current, err := s.GetSwarmPassTrace(ctx, "one", 3)
	mustNil(t, err)
	if !current.Current || current.CurrentExecution != "waiting" || current.Goal != "Second trace goal" || len(current.Steps) != 1 || current.Steps[0].Visit.Sequence != 3 {
		t.Fatalf("current trace lost exact pass status: %+v", current)
	}
	for _, request := range []struct {
		swarm string
		pass  int64
	}{{"one", 2}, {"one", 0}, {"one", -1}, {"two", 1}, {"missing", 1}} {
		if _, err := s.GetSwarmPassTrace(ctx, request.swarm, request.pass); !errors.Is(err, ErrNotFound) {
			t.Fatalf("non-pass or foreign sequence accepted as a pass: %+v, %v", request, err)
		}
	}
}

func TestPassTraceRetainsGraphAsOriginalTextRatherThanAssumingJSON(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	mustStart(t, s, "release")
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	encoded, err := json.Marshal(trace.Graph)
	mustNil(t, err)
	var text string
	mustNil(t, json.Unmarshal(encoded, &text))
	swarm, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	if text != string(swarm.Graph) || text != startRelease {
		t.Fatal("trace rewrote or base64-encoded the original pinned graph document")
	}
}

func TestPassTraceRetainsExactInstructionAndAllReceiptsWithoutReconciliation(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "trace-instruction")
	intent, err := s.RecordActorStepAttestationDelivery(ctx, a, startOrigin, startCommit, instructionDeliveryInput(a), allow)
	mustNil(t, err)
	now := time.Now().UTC()
	first, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, first, "unconfirmed", "response lost", now, allow))
	mustNil(t, s.RecordActorDeliveryWait(ctx, intent.ID, "probe only", allow))
	now = now.Add(time.Minute)
	second, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, second, "duplicate", "retained confirmation", now, allow))
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	if len(trace.Steps) != 1 || trace.Steps[0].Instruction == nil {
		t.Fatal("trace omitted exact retained instruction")
	}
	delivery := trace.Steps[0].Instruction
	if delivery.Intent != intent || len(delivery.Attempts) != 2 || delivery.Attempts[0].Attempt != first || delivery.Attempts[0].Outcome != "unconfirmed" || delivery.Attempts[1].Attempt != second || delivery.Attempts[1].Outcome != "duplicate" {
		t.Fatalf("trace reconstructed or collapsed delivery history: %+v", delivery)
	}
	if trace.Steps[0].Visit.State != "unconfirmed" || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("trace read used a retained receipt as permission to reconcile source state")
	}
}

func TestPassTraceIncludesOriginalActorAnswerAndItsDeliveryAttempts(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	mustStart(t, s, "release")
	q := passContextQuestion(t, s)
	_, err := s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: q.ID, UserID: "u", Response: "Keep every migration row."}, allow)
	mustNil(t, err)
	input := ActorDeliveryInput{SwarmID: "one", SourceKind: "answer", SourceID: q.ID, ActorUID: "a", Incarnation: 1, AtespaceUID: "native-one", JournalID: strings.Repeat("a", 64), Payload: `{"to":"a","body":"Keep every migration row.","from":"u","waitSec":0}`}
	intent, err := s.PrepareActorDelivery(ctx, input, allow)
	mustNil(t, err)
	now := time.Now().UTC()
	attempt, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, attempt, "not_sent", "not ready", now, allow))
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	found := false
	for _, item := range trace.Requests {
		if item.Request.ID != q.ID {
			continue
		}
		found = true
		if item.CreatedDuring == nil || item.CreatedDuring.Pass != 1 || item.Request.Response == nil || *item.Request.Response != "Keep every migration row." || item.Delivery == nil || item.Delivery.Intent != intent || len(item.Delivery.Attempts) != 1 || item.Delivery.Attempts[0].Attempt != attempt || item.Delivery.Attempts[0].Outcome != "not_sent" {
			t.Fatalf("trace dropped the actual answer frame or its history: %+v", item)
		}
	}
	if !found || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("trace omitted the answer or retried delivery")
	}
}

func TestPassTraceCancelledReadReturnsNoPartialMaterial(t *testing.T) {
	s := dispatchStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(trace, SwarmPassTrace{}) {
		t.Fatalf("canceled read exposed partial trace material: %+v, %v", trace, err)
	}
}

func TestPassTraceIncludesChildInstructionsAndReportsWithoutJoining(t *testing.T) {
	ctx := context.Background()
	s, parent, executor := childFixture(t, "all", 1, 0)
	child := finishChild(t, s, parent, executor, "trace-child", "success")
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	if len(trace.Steps) != 1 || trace.Steps[0].Children == nil {
		t.Fatal("trace dropped retained child group")
	}
	group := trace.Steps[0].Children
	if group.Parent != parent || len(group.Children) != 1 || group.Children[0] != child || group.State != "open" || group.Evaluation.State != "ready_success" {
		t.Fatalf("trace lost actual child payload/report or finalized the group: %+v", group)
	}
	if trace.Steps[0].Instruction != nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("trace invented an unsaved legacy frame or changed join state")
	}
}

func TestPassTraceSeparatesReportedOutcomeFromHeldPolicyAndSignedEvidence(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	resolution, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result")
	mustNil(t, err)
	if resolution.Applied {
		t.Fatal("fixture unexpectedly advanced before policy confirmation")
	}
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	step := trace.Steps[0]
	if step.Completion == nil || *step.Completion != observation.Intent || step.Visit.Outcome != nil || step.ResultObservation == nil || step.ResultObservation.State != "observed" || step.ResultEvidence == nil || !reflect.DeepEqual(*step.ResultEvidence, signed) {
		t.Fatal("trace conflated reported work, actual outcome, policy permission or verified evidence")
	}
	found := false
	for _, item := range step.PolicyCandidates {
		if item.Boundary == "result" {
			found = reflect.DeepEqual(item, candidate)
		}
	}
	if !found || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("trace recomputed or omitted the immutable original policy decision")
	}
}
