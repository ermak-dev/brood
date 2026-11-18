package store

// FEATURE: Eval postconditions and non-graph refusals (#293).
// SPEC: Ignored waiting-request writes cannot grant execution; repeated nodes
// use retained occurrence; actor/policy questions never manufacture approvals;
// outstanding children require separate owner authority and retain all holds.
// IMPLEMENTATION: Проверка retained postconditions и отдельный owner callback.
// RESULT: IMPLEMENTED — ignored writes откатываются; child holds сохраняются.
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

func TestSwarmEvalStartRejectsIgnoredPolicySnapshot(t *testing.T) {
	ctx := context.Background()
	s, in := evalFixture(t)
	_, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER ignore_eval_policy BEFORE INSERT ON swarm_pass_policies WHEN NEW.swarm_id='two' BEGIN SELECT RAISE(IGNORE); END`)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
	if err == nil {
		t.Fatal("start succeeded without admitted policy snapshot")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("failed policy snapshot left partial start")
	}
}

func TestSwarmEvalStartRejectsIgnoredWaitingRequest(t *testing.T) {
	ctx := context.Background()
	s, in := evalFixture(t)
	_, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER ignore_eval_request BEFORE INSERT ON swarm_requests WHEN NEW.swarm_id='two' BEGIN SELECT RAISE(IGNORE); END`)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
	if err == nil {
		t.Fatal("start succeeded without its waiting question")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("failed start left graph/pass history")
	}
}

const evalLoopGraph = `apiVersion: brood.dev/v1alpha1
name: approve
start: entry
exit: done
nodes:
  entry:
    type: human
    question: Begin?
    options:
      - {label: Begin, outcome: success}
  ask:
    type: human
    question: Continue?
    options:
      - {label: Again, outcome: again}
      - {label: Done, outcome: success}
  done: {type: exit}
edges:
  - {from: entry, to: ask}
  - {from: ask, to: ask, label: Again}
  - {from: ask, to: done, label: Done}
goalGates: [done]
`

func evalLoopFixture(t *testing.T) (*Store, SwarmEvalReplayInput, []string) {
	t.Helper()
	ctx := context.Background()
	s := startStore(t)
	publish(t, s, "one", map[string]string{"approve": evalLoopGraph})
	_, err := startOne(s, "approve")
	mustNil(t, err)
	entry := openNodeRequestOf(t, s, 1)
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: entry.ID, UserID: "u", Outcome: "success"}, allow)
	mustNil(t, err)
	var referenceIDs []string
	for i, outcome := range []string{"again", "success"} {
		req := openNodeRequestOf(t, s, int64(i+2))
		referenceIDs = append(referenceIDs, req.ID)
		_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: req.ID, UserID: "u", Outcome: outcome}, allow)
		mustNil(t, err)
	}
	mustNil(t, s.SetProjectRepository(ctx, "two", startOrigin))
	publish(t, s, "two", map[string]string{"approve": evalLoopGraph})
	_, err = s.CreateSwarmEvalBinding(ctx, SwarmEvalBindingInput{CandidateSwarmID: "two", ReferenceSwarmID: "one", ReferencePass: 1, LauncherID: "root", Definition: "approve", Model: "selected-model-1"}, evalAuthorize)
	mustNil(t, err)
	_, err = s.StartSwarmEval(ctx, "two", "root", evalAuthorize)
	mustNil(t, err)
	requests, err := s.SwarmRequests(ctx, "two")
	mustNil(t, err)
	_, err = s.ApplySwarmEvalReplay(ctx, SwarmEvalReplayInput{CandidateSwarmID: "two", RequestID: requests[0].ID, LauncherID: "root"}, evalAuthorize, nil)
	mustNil(t, err)
	requests, err = s.SwarmRequests(ctx, "two")
	mustNil(t, err)
	for _, r := range requests {
		if r.State == "open" {
			return s, SwarmEvalReplayInput{CandidateSwarmID: "two", RequestID: r.ID, LauncherID: "root"}, referenceIDs
		}
	}
	t.Fatal("no loop request")
	return nil, SwarmEvalReplayInput{}, nil
}

func TestSwarmEvalReplayUsesRetainedOccurrence(t *testing.T) {
	ctx := context.Background()
	s, in, ids := evalLoopFixture(t)
	for i, want := range []string{"again", "success"} {
		if i > 0 {
			requests, err := s.SwarmRequests(ctx, "two")
			mustNil(t, err)
			for _, r := range requests {
				if r.State == "open" {
					in.RequestID = r.ID
				}
			}
		}
		result, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
		mustNil(t, err)
		if result.Answer == nil || result.Answer.Outcome != want || result.Answer.Occurrence != i+1 || result.Answer.ReferenceRequestID != ids[i] {
			t.Fatalf("wrong occurrence: %+v", result)
		}
	}
}

func TestSwarmEvalReplayRejectsIgnoredSuccessorQuestion(t *testing.T) {
	ctx := context.Background()
	s, in, _ := evalLoopFixture(t)
	_, err := s.db.Exec(`CREATE TRIGGER ignore_eval_question BEFORE INSERT ON swarm_requests WHEN NEW.swarm_id='two' BEGIN SELECT RAISE(IGNORE); END`)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	_, err = s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	if err == nil {
		t.Fatal("replay succeeded without successor question")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("failed replay left partial transition")
	}
}

func evalActorFixture(t *testing.T, policy bool) (*Store, SwarmEvalReplayInput) {
	t.Helper()
	ctx := context.Background()
	s, in := evalFixture(t)
	in.LauncherID = "root"
	graph := strings.Replace(startBuildFirst, "name: build-first", "name: approve", 1)
	if policy {
		graph = strings.Replace(graph, "role: build,", "role: build, actionClasses: [work],", 1)
		_, err := s.SetProjectWorkflowPolicy(ctx, "root", "two", 0, []workflowpolicy.Rule{{ID: "human", Actions: []string{"work"}, Require: workflowpolicy.Human}}, allow)
		mustNil(t, err)
	}
	publish(t, s, "two", map[string]string{"approve": graph})
	_, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	_, err = s.ReserveManagedActor(ctx, managedRequest("candidate", "two", 202))
	mustNil(t, err)
	setActor(t, s, "candidate", "phase='running', stage='done', operation='start'")
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "two", Role: "build", ActorUID: "candidate"}))
	_, err = s.StartSwarmEval(ctx, "two", "root", evalAuthorize)
	mustNil(t, err)
	var request SwarmRequest
	if policy {
		candidate, err := s.RecordPipelinePolicyCandidate(ctx, "two", 1, "entry", "eval-policy")
		mustNil(t, err)
		request, err = s.GetSwarmRequest(ctx, "two", policyRequestPrefix+candidate.ID)
		mustNil(t, err)
	} else {
		request, err = s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "two", ActorUID: "candidate", ActorIncarnation: 1, Kind: "text", Question: "Need a new human decision"})
		mustNil(t, err)
	}
	return s, SwarmEvalReplayInput{CandidateSwarmID: "two", RequestID: request.ID, LauncherID: "root"}
}

func TestSwarmEvalReplayStopsActorAndPolicyQuestionsWithoutApproval(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(map[bool]string{false: "actor", true: "policy"}[policy], func(t *testing.T) {
			ctx := context.Background()
			s, in := evalActorFixture(t, policy)
			_, err := s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "two", RequestID: in.RequestID, UserID: "root", Response: "yes", ExpectedRevision: 1}, allow)
			if !errors.Is(err, ErrSwarmEvalConflict) {
				t.Fatalf("live human answer: %v", err)
			}
			confirmations := dumpTables(t, s, "pipeline_policy_confirmations")
			result, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
			mustNil(t, err)
			if result.State != "stopped" || result.Reason == "" || result.Answer != nil {
				t.Fatalf("non-graph question replayed: %+v", result)
			}
			req, err := s.GetSwarmRequest(ctx, "two", in.RequestID)
			mustNil(t, err)
			if req.AnsweredBy != "" || req.Response != nil || req.Outcome != nil || dumpTables(t, s, "pipeline_policy_confirmations") != confirmations {
				t.Fatal("fabricated human approval")
			}
		})
	}
}

func TestSwarmEvalStopWithChildrenRequiresPositiveOwnerCallbackAndRetainsHolds(t *testing.T) {
	ctx := context.Background()
	s, in := evalActorFixture(t, false)
	attempt, err := s.ReserveActorStep(ctx, "two", "eval-parent", "swarm:two")
	mustNil(t, err)
	mustNil(t, s.RecordActorStepAttestation(ctx, attempt, startOrigin, startCommit))
	op, err := s.CurrentActorOperation(ctx, "candidate")
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	parent := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "two", SwarmID: "two", AtespaceUID: "native-two", ActorUID: "candidate", Incarnation: 1}, Pass: 1, Sequence: 1, RequestID: "eval-parent"}
	_, err = s.CreateSwarmChildJoin(ctx, parent, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	_, err = s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("child interruption without owner: %v", err)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("denied stop changed holds")
	}
	held := dumpTables(t, s, "managed_actors", "coordination_claims", "swarm_child_joins", "swarm_child_tasks")
	called := false
	owner := func(tx context.Context, action string, b SwarmEvalBinding) error {
		called = true
		if action != "interrupt_eval_children" {
			t.Fatalf("owner action: %s", action)
		}
		return nil
	}
	result, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, owner)
	mustNil(t, err)
	if !called || result.State != "stopped" {
		t.Fatal("owner callback not used")
	}
	if dumpTables(t, s, "managed_actors", "coordination_claims", "swarm_child_joins", "swarm_child_tasks") != held {
		t.Fatal("stop changed child or physical holds")
	}
	step, err := s.GetSwarmStep(ctx, "two", 1)
	mustNil(t, err)
	if step.State != "unconfirmed" {
		t.Fatalf("child parent uncertainty lost: %+v", step)
	}
	_, err = s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, nil)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("child stop retry skipped owner authority: %v", err)
	}
	denied := func(context.Context, string, SwarmEvalBinding) error { return ErrSwarmRequestDenied }
	_, err = s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, denied)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("child stop retry ignored revoked owner callback: %v", err)
	}
	called = false
	retry, err := s.ApplySwarmEvalReplay(ctx, in, evalAuthorize, owner)
	mustNil(t, err)
	if !called || !reflect.DeepEqual(retry, result) {
		t.Fatal("reauthorized child stop retry changed history")
	}
}
