package store

// BUG: отказ review оставлял проход без terminal resolution; unknown нельзя считать отказом.
// HYPOTHESIS: известный refused review должен завершать pass, сохраняя отсутствие actor outcome.
// SOLUTION: terminal resolution читает реальные receipt/evidence/refusal; unknown
// остаётся held, manual/child fences общие с обычным result collector.
// RESULT: FIXED — full Go и focused race; реальные reports/outcomes сохранены.
// STATUS: FIXED
// Полный Store race ограничен timeout 30m, не объявляется прошедшим.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

// Work after a review is a new producer event, even if its final H happens to
// equal the previously reviewed H. That review was not the next review of this work.
func TestPolicyInlineReviewCannotAuthorizeLaterOrdinaryWork(t *testing.T) {
	for _, head := range []string{strings.Repeat("ef", 20), strings.Repeat("cd", 20)} {
		t.Run(head[:2], func(t *testing.T) {
			graph := strings.Replace(inlineReviewGraph, "  land: {type:", "  revise: {type: actor, role: build, actionClasses: [work], prompt: revise}\n  land: {type:", 1)
			graph = strings.ReplaceAll(graph, "from: review, to: land", "from: review, to: revise")
			graph = strings.Replace(graph, "  - {from: land, to: done}", "  - {from: revise, to: land}\n  - {from: land, to: done}", 1)
			s := inlineReviewStore(t, graph, "reviewer")
			inlineApprove(t, s, 1)
			inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
			result := inlineApprove(t, s, 2)
			if !result.Applied || result.Rejected {
				t.Fatalf("could not enter ordinary work: %+v", result)
			}
			inlineObserveActor(t, s, "success", head, []string{"go/brood/store/revised.go"}, startCommit, "revise-work")
			result = inlineApprove(t, s, 3)
			w, err := s.GetSwarm(context.Background(), "one")
			mustNil(t, err)
			if !result.Applied || !result.Rejected || w.Execution != "failed" || stepCount(t, s) != 3 {
				t.Fatalf("old review authorized new work: %+v execution=%s", result, w.Execution)
			}
		})
	}
}

func TestPolicyInlineReviewUnchangedArtifactKeepsOriginalAuthorAcrossReviewVisits(t *testing.T) {
	graph := strings.Replace(inlineReviewGraph, "  land: {type:", "  review-again: {type: actor, role: review, actionClasses: [review], prompt: review again}\n  land: {type:", 1)
	graph = strings.ReplaceAll(graph, "from: review, to: land", "from: review, to: review-again")
	graph = strings.Replace(graph, "  - {from: land, to: done}", "  - {from: review-again, to: land}\n  - {from: land, to: done}", 1)
	s := inlineReviewStore(t, graph, "reviewer")
	inlineApprove(t, s, 1)
	inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
	result := inlineApprove(t, s, 2)
	if !result.Applied || result.Rejected {
		t.Fatalf("latest reviewer was mistaken for work author: %+v", result)
	}
	requirements, err := s.PipelinePolicyReviewRequirements(context.Background(), "one", result.CandidateID)
	mustNil(t, err)
	if len(requirements) != 1 || requirements[0].ProducerUID != "a" {
		t.Fatalf("work lineage changed after observation: %+v", requirements)
	}
	inlineObserveActor(t, s, "success", strings.Repeat("cd", 20), []string{}, strings.Repeat("cd", 20), "review-again")
	result = inlineApprove(t, s, 3)
	if !result.Applied || result.Rejected || result.Decision.State != workflowpolicy.Allowed {
		t.Fatalf("unchanged artifact required invented third reviewer: %+v", result)
	}
}

func TestPolicyInlineReviewUnrelatedActionsDoNotCreateReviewAuthority(t *testing.T) {
	graph := strings.Replace(inlineReviewGraph, "actionClasses: [landing]", "actionClasses: [deploy]", 1)
	s := inlineReviewStoreRules(t, graph, "a", []workflowpolicy.Rule{{ID: "landing-review", Actions: []string{"landing"}, Require: workflowpolicy.IndependentReview, ReviewRole: "review"}})
	result := inlineApprove(t, s, 1)
	if !result.Applied || result.Rejected {
		t.Fatalf("absent landing action invented independent review: %+v", result)
	}
	_, err := s.ReserveActorStep(context.Background(), "one", "unprotected-action-review", "swarm:one")
	mustNil(t, err)
}

func TestPolicyInlineReviewUnrelatedPathsDoNotCreateReviewAuthority(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStoreRules(t, inlineReviewGraph, "a", []workflowpolicy.Rule{{ID: "docs-review", Paths: []string{"docs/**"}, Actions: []string{"landing"}, Require: workflowpolicy.IndependentReview, ReviewRole: "review"}})
	result := inlineApprove(t, s, 1)
	if !result.Applied || result.Rejected {
		t.Fatalf("unrelated protected paths invented an independence obligation: %+v", result)
	}
	rs, err := s.PipelinePolicyReviewRequirements(ctx, "one", result.CandidateID)
	mustNil(t, err)
	if len(rs) != 0 {
		t.Fatalf("unrelated rule retained a review requirement: %+v", rs)
	}
	_, err = s.ReserveActorStep(ctx, "one", "unprotected-review", "swarm:one")
	mustNil(t, err)
}

func TestPolicyInlineReviewPathRequirementDispatchesActualNextReviewer(t *testing.T) {
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer", workflowpolicy.Rule{ID: "review-storage", Paths: []string{"go/brood/store/**"}, Require: workflowpolicy.IndependentReview, ReviewRole: "review"})
	first := inlineApprove(t, s, 1)
	if !first.Applied || first.Rejected {
		t.Fatalf("review requirement prevented the graph reviewer from running: %+v", first)
	}
	inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
	result := inlineApprove(t, s, 2)
	if !result.Applied || result.Rejected || result.Decision.State != workflowpolicy.Allowed {
		t.Fatalf("actual path review not consumed: %+v", result)
	}
}

func TestPolicyInlineReviewWrongConfiguredRoleIsTerminal(t *testing.T) {
	graph := strings.Replace(inlineReviewGraph, "role: review, actionClasses: [review]", "role: build, actionClasses: [review]", 1)
	s := inlineReviewStore(t, graph, "reviewer")
	result := inlineApprove(t, s, 1)
	w, err := s.GetSwarm(context.Background(), "one")
	mustNil(t, err)
	if !result.Applied || !result.Rejected || w.Execution != "failed" || stepCount(t, s) != 1 {
		t.Fatalf("wrong configured next role advanced: %+v execution=%s", result, w.Execution)
	}
}

func TestPolicyInlineReviewResultRefusalIsTerminalButUnknownStaysHeld(t *testing.T) {
	for _, state := range []string{"refused", "unknown"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
			inlineApprove(t, s, 1)
			inlineObserveActor(t, s, "success", strings.Repeat("cd", 20), []string{}, strings.Repeat("cd", 20), "unsettled-review", state)
			before := dumpTables(t, s, "swarm_steps", "pipeline_policy_review_assignments", "pipeline_result_observations")
			result, err := s.ResolvePipelinePolicyReview(ctx, "one", 2)
			mustNil(t, err)
			w, err := s.GetSwarm(ctx, "one")
			mustNil(t, err)
			step, err := s.GetSwarmStep(ctx, "one", 2)
			mustNil(t, err)
			if state == "refused" {
				if !result.Applied || !result.Rejected || w.Execution != "failed" || step.Outcome == nil || *step.Outcome != "success" || step.Report != "Actual inline review: success" || !strings.Contains(step.Detail, "refused") {
					t.Fatalf("root refusal not durably resolved: %+v step=%+v execution=%s", result, step, w.Execution)
				}
			} else {
				if result.Applied || result.Rejected || w.Execution == "failed" || before != dumpTables(t, s, "swarm_steps", "pipeline_policy_review_assignments", "pipeline_result_observations") {
					t.Fatal("unknown result became negative or changed history")
				}
				if _, err := s.ReserveActorStep(ctx, "one", "unknown-replacement", "swarm:one"); err == nil {
					t.Fatal("unknown review assigned again")
				}
			}
		})
	}
}

func TestPolicyInlineReviewReboundIncarnationDoesNotReplaceAssignment(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	first := inlineApprove(t, s, 1)
	original, err := s.GetPipelinePolicyReviewAssignment(ctx, "one", first.CandidateID, "land-review")
	mustNil(t, err)
	setActor(t, s, "reviewer", "incarnation=incarnation+1")
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "review", ActorUID: "reviewer"}))
	_, err = s.ReserveActorStep(ctx, "one", "replacement-incarnation", "swarm:one")
	if !errors.Is(err, ErrActorStepRefused) {
		t.Fatalf("rebound review did not resolve known unavailability: %v", err)
	}
	w, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	retained, err := s.GetPipelinePolicyReviewAssignment(ctx, "one", first.CandidateID, "land-review")
	mustNil(t, err)
	if w.Execution != "failed" || original != retained {
		t.Fatal("review assignment silently replaced or remained unresolved")
	}
}

func TestPolicyInlineReviewTerminalReservationQueuesOneFinish(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	inlineApprove(t, s, 1)
	_, err := s.SetTelegramBot(ctx, "root", "100:fixture-token", "root_brood_bot", "")
	mustNil(t, err)
	linkTelegramAs(t, s, "root", "root_brood_bot", 4242, "root_tg")
	_, err = s.SetTelegramEvents(ctx, "root", []string{"pass_finished"})
	mustNil(t, err)
	setActor(t, s, "reviewer", "incarnation=incarnation+1")
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "review", ActorUID: "reviewer"}))
	_, err = s.ReserveActorStep(ctx, "one", "terminal-review", "swarm:one")
	if !errors.Is(err, ErrActorStepRefused) {
		t.Fatalf("review was not refused: %v", err)
	}
	var n int
	mustNil(t, s.db.QueryRow(`SELECT COUNT(*) FROM telegram_outbox WHERE kind='pass_finished' AND user_id='root'`).Scan(&n))
	if n != 1 {
		t.Fatalf("one review failure queued %d pass-finished events", n)
	}
}

func TestPolicyInlineReviewReservationRefusalIsTerminal(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	inlineApprove(t, s, 1)
	_, err := s.AcquireActorOperation(ctx, "reviewer", 1, "busy-review-workspace", "existing-caller", WorkspaceAttestOperation)
	mustNil(t, err)
	_, err = s.ReserveActorStep(ctx, "one", "busy-review", "swarm:one")
	if !errors.Is(err, ErrActorStepRefused) {
		t.Fatalf("expected known reservation refusal: %v", err)
	}
	w, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	if w.Execution != "failed" {
		t.Fatal("known reviewer reservation refusal left pass running")
	}
}

func TestPolicyInlineReviewKnownRefusalIsTerminalWithoutInventedOutcome(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	inlineApprove(t, s, 1)
	a, err := s.ReserveActorStep(ctx, "one", "refused-review", "swarm:one")
	mustNil(t, err)
	mustNil(t, s.RefuseActorStep(ctx, a, "review transport was definitely not sent"))
	w, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	review, err := s.GetSwarmStep(ctx, "one", 2)
	mustNil(t, err)
	work, err := s.GetSwarmStep(ctx, "one", 1)
	mustNil(t, err)
	if w.Execution != "failed" || review.State != "refused" || review.Outcome != nil || review.Report != "" || !strings.Contains(review.Detail, "not sent") || work.Outcome == nil || *work.Outcome != "success" {
		t.Fatalf("known review refusal not durably resolved: execution=%s review=%+v", w.Execution, review)
	}
	if _, err := s.ReserveActorStep(ctx, "one", "replacement", "swarm:one"); err == nil {
		t.Fatal("terminal review automatically replaced")
	}
}

func TestPolicyInlineReviewAfterHumanUsesNewEntryApprovalAndExactArtifact(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(inlineReviewGraph, "  done: {type: exit}", "  ask:\n    type: human\n    question: Review?\n    options:\n      - {label: Continue, outcome: success}\n  done: {type: exit}", 1)
	graph = strings.Replace(graph, "  - {from: build, to: review}", "  - {from: build, to: ask}\n  - {from: ask, to: review}", 1)
	s := inlineReviewStore(t, graph, "reviewer")
	result, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1)
	mustNil(t, err)
	c, err := s.GetPipelinePolicyCandidateByID(ctx, "one", result.CandidateID)
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(c, "u", "yes"), allow)
	mustNil(t, err)
	result, err = s.ResolvePipelinePolicyHuman(ctx, "one", 1)
	mustNil(t, err)
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	for _, r := range requests {
		if r.Origin == "node" {
			_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: r.ID, UserID: "u", Outcome: "success"}, allow)
			mustNil(t, err)
		}
	}
	if _, err := s.PipelineActorStepSource(ctx, "one", 3); !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatalf("unassigned review preparation selected pass source: %v", err)
	}
	_, err = s.ReserveActorStep(ctx, "one", "before-entry-approval", "swarm:one")
	if !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatalf("graph answer supplied review-entry approval: %v", err)
	}
	entry, err := s.GetPipelinePolicyCandidate(ctx, "one", 3, "entry")
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(entry, "u", "yes"), allow)
	mustNil(t, err)
	inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
	result = inlineApprove(t, s, 3)
	if !result.Applied || result.Rejected {
		t.Fatalf("post-human review did not authorize landing: %+v", result)
	}
}

func TestPolicyInlineReviewHumanGraphGateKeepsArtifactAndDoesNotImportApproval(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "stale"}[stale], func(t *testing.T) {
			ctx := context.Background()
			graph := strings.Replace(inlineReviewGraph, "  done: {type: exit}", "  ask:\n    type: human\n    question: Land?\n    options:\n      - {label: Continue, outcome: success}\n  done: {type: exit}", 1)
			graph = strings.ReplaceAll(graph, "from: review, to: land", "from: review, to: ask")
			graph = strings.Replace(graph, "  - {from: land, to: done}", "  - {from: ask, to: land}\n  - {from: land, to: done}", 1)
			s := inlineReviewStore(t, graph, "reviewer")
			inlineApprove(t, s, 1)
			inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
			result, err := s.ResolvePipelinePolicyHuman(ctx, "one", 2)
			mustNil(t, err)
			c, err := s.GetPipelinePolicyCandidateByID(ctx, "one", result.CandidateID)
			mustNil(t, err)
			_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(c, "u", "yes"), allow)
			mustNil(t, err)
			result, err = s.ResolvePipelinePolicyHuman(ctx, "one", 2)
			mustNil(t, err)
			if !result.Applied || result.Rejected {
				t.Fatalf("review did not reach graph gate: %+v", result)
			}
			requests, err := s.SwarmRequests(ctx, "one")
			mustNil(t, err)
			for _, r := range requests {
				if r.Origin == "node" {
					_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: r.ID, UserID: "u", Outcome: "success"}, allow)
					mustNil(t, err)
				}
			}
			_, err = s.ReserveActorStep(ctx, "one", "graph-is-not-approval", "swarm:one")
			if !errors.Is(err, ErrPipelinePolicyPending) {
				t.Fatalf("graph answer imported policy approval: %v", err)
			}
			entry, err := s.GetPipelinePolicyCandidate(ctx, "one", 4, "entry")
			mustNil(t, err)
			if entry.Preview.Input.Initial || entry.Preview.Input.Evidence.ProducerUID != "a" || entry.Preview.Input.Evidence.Artifact != strings.Repeat("cd", 20) || entry.Preview.Decision.State != workflowpolicy.WaitingHuman {
				t.Fatalf("human route lost work lineage: %+v", entry)
			}
			_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(entry, "u", "yes"), allow)
			mustNil(t, err)
			if stale {
				setActor(t, s, "reviewer", "incarnation=incarnation+1")
			}
			_, err = s.ReserveActorStep(ctx, "one", "inline-graph-land", "swarm:one")
			if stale {
				if !errors.Is(err, ErrActorStepRefused) {
					t.Fatalf("stale prior reviewer did not terminally refuse entry: %v", err)
				}
				w, err := s.GetSwarm(ctx, "one")
				mustNil(t, err)
				if w.Execution != "failed" {
					t.Fatal("unavailable completed reviewer left landing pending")
				}
			} else {
				mustNil(t, err)
			}
		})
	}
}
