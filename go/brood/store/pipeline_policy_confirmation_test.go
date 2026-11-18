package store

// Подтверждение связано с immutable candidate, но не является dispatch grant.
// Ответ и повтор не меняют actor outcome, граф или право на project credentials.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/workflowpolicy"
)

func policyConfirmationFixture(t *testing.T) (*Store, PipelinePolicyCandidate) {
	t.Helper()
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "human-candidate")
	mustNil(t, err)
	return s, candidate
}

func policyAnswer(candidate PipelinePolicyCandidate, user, answer string) PipelinePolicyAnswer {
	return PipelinePolicyAnswer{SwarmID: candidate.Preview.SwarmID, CandidateID: candidate.ID, UserID: user, ExpectedRevision: 1, Answer: answer}
}

func TestPolicyConfirmationRecordsOneExactAnswerWithoutApplyingCandidate(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	before := dumpTables(t, s, append(executionTables, "pipeline_policy_candidates")...)
	in := policyAnswer(candidate, "u", "yes")
	first, err := s.AnswerPipelinePolicy(ctx, in, allow)
	mustNil(t, err)
	if first.SwarmID != "one" || first.CandidateID != candidate.ID || first.UserID != "u" || first.Answer != "yes" || first.Revision != 2 || first.AnsweredAt == "" {
		t.Fatalf("confirmation lost its exact scope/answer: %+v", first)
	}
	again, err := s.AnswerPipelinePolicy(ctx, in, allow)
	mustNil(t, err)
	if !reflect.DeepEqual(first, again) {
		t.Fatal("retry changed first response identity/timestamp")
	}
	if dumpTables(t, s, append(executionTables, "pipeline_policy_candidates")...) != before {
		t.Fatal("confirmation applied a candidate, created a node or changed actual outcome")
	}
	for _, query := range []string{
		`UPDATE pipeline_policy_confirmations SET answer='no'`,
		`DELETE FROM pipeline_policy_confirmations`,
		`INSERT OR REPLACE INTO pipeline_policy_confirmations SELECT * FROM pipeline_policy_confirmations`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "policy confirmation history is immutable") {
			t.Fatalf("confirmation history fence: %v", err)
		}
	}
	stopOne(t, s)
	historical, err := s.AnswerPipelinePolicy(ctx, in, allow)
	mustNil(t, err)
	swarm, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	if !reflect.DeepEqual(first, historical) || swarm.Execution != "stopped" {
		t.Fatal("lost response retry changed history or resumed stopped work")
	}
}

func TestPolicyConfirmationNoIsRetainedAndCannotBecomeYes(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	in := policyAnswer(candidate, "u", "no")
	first, err := s.AnswerPipelinePolicy(ctx, in, allow)
	mustNil(t, err)
	in.Answer = "yes"
	if _, err := s.AnswerPipelinePolicy(ctx, in, allow); !errors.Is(err, ErrPolicyConfirmationConflict) {
		t.Fatalf("rejection was overwritten: %v", err)
	}
	got, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID)
	mustNil(t, err)
	if !reflect.DeepEqual(first, got) || got.Answer != "no" || stepCount(t, s) != 1 {
		t.Fatal("rejection lost its original answer or advanced graph")
	}
}

func TestPolicyConfirmationRechecksSessionAndCurrentManager(t *testing.T) {
	for _, who := range []string{"viewer", "actor", "session", "membership", "disabled"} {
		t.Run(who, func(t *testing.T) {
			ctx := context.Background()
			s, candidate := policyConfirmationFixture(t)
			in := policyAnswer(candidate, "u", "yes")
			authorize := allow
			switch who {
			case "viewer":
				in.UserID = "v"
			case "actor":
				in.UserID = "a"
			case "session":
				authorize = func(context.Context) bool { return false }
			case "membership":
				_, err := s.db.Exec(`DELETE FROM project_members WHERE project_id='one' AND user_id='u'`)
				mustNil(t, err)
			case "disabled":
				_, err := s.db.Exec(`UPDATE users SET disabled=1 WHERE id='u'`)
				mustNil(t, err)
			}
			if _, err := s.AnswerPipelinePolicy(ctx, in, authorize); !errors.Is(err, ErrSwarmRequestDenied) {
				t.Fatalf("stale/foreign authority accepted: %v", err)
			}
			if _, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused answer wrote history: %v", err)
			}
		})
	}
}

func TestPolicyConfirmationRejectsForeignScopeAndLateFirstAnswer(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	in := policyAnswer(candidate, "root", "yes")
	in.SwarmID = "two"
	if _, err := s.AnswerPipelinePolicy(ctx, in, allow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-swarm candidate ID accepted: %v", err)
	}
	stopOne(t, s)
	in.SwarmID = "one"
	if _, err := s.AnswerPipelinePolicy(ctx, in, allow); err == nil {
		t.Fatal("late first answer after stop was accepted")
	}
	if _, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("late answer invented historical approval")
	}
}

func TestPolicyConfirmationInitialActionRequiresExplicitHumanObligation(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [work],", 1)
	for _, requirement := range []string{workflowpolicy.Human, workflowpolicy.Deny} {
		t.Run(requirement, func(t *testing.T) {
			s := policyPreviewStore(t, graph, []workflowpolicy.Rule{{ID: "work", Actions: []string{"work"}, Require: requirement}})
			candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "entry", "initial-action")
			mustNil(t, err)
			_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
			if requirement == workflowpolicy.Human {
				mustNil(t, err)
			} else if !errors.Is(err, ErrPolicyConfirmationConflict) {
				t.Fatalf("confirmation overrode denied candidate: %v", err)
			}
			if stepOf(t, s, 1).State != "pending" {
				t.Fatal("recording initial approval dispatched work")
			}
		})
	}
}

func TestPolicyConfirmationPreservesPinnedPolicyAndOwnerIntervention(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	_, err := s.SetProjectWorkflowPolicy(ctx, "root", "one", 1, []workflowpolicy.Rule{{ID: "new-deny", Paths: []string{"**"}, Require: workflowpolicy.Deny}}, allow)
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
	mustNil(t, err)

	other, pending := policyConfirmationFixture(t)
	actor, err := other.GetManagedActor(ctx, "a")
	mustNil(t, err)
	_, err = other.AcquireOwnerActorOperation(ctx, actor, "confirmation-owner-control", "u", "slash", "u", func(context.Context) error { return nil })
	mustNil(t, err)
	if _, err := other.AnswerPipelinePolicy(ctx, policyAnswer(pending, "u", "yes"), allow); !errors.Is(err, ErrPipelineVisitInterrupted) {
		t.Fatalf("owner intervention was bypassed: %v", err)
	}
}

func TestPolicyConfirmationRevisionAndAnswerAreStrict(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	for _, revision := range []int64{-1, 0, 2} {
		in := policyAnswer(candidate, "u", "yes")
		in.ExpectedRevision = revision
		if _, err := s.AnswerPipelinePolicy(ctx, in, allow); !errors.Is(err, ErrPolicyConfirmationConflict) {
			t.Fatalf("revision %d: %v", revision, err)
		}
	}
	for _, answer := range []string{"", "Yes", "approve", "yes\n"} {
		if _, err := s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", answer), allow); !errors.Is(err, ErrSwarmRequestInvalid) {
			t.Fatalf("invalid answer %q: %v", answer, err)
		}
	}
}

func TestPolicyConfirmationSQLRechecksAuthorityAndScope(t *testing.T) {
	for _, fault := range []string{"viewer", "foreign", "stopped"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			s, candidate := policyConfirmationFixture(t)
			user, swarm := "u", "one"
			switch fault {
			case "viewer":
				user = "v"
			case "foreign":
				swarm, user = "two", "root"
			case "stopped":
				stopOne(t, s)
			}
			_, err := s.db.Exec(`INSERT INTO pipeline_policy_confirmations VALUES(?,?,2,?,'yes','now')`, candidate.ID, swarm, user)
			if err == nil || !strings.Contains(err.Error(), "requires current manager and candidate") {
				t.Fatalf("raw SQL %s confirmation accepted: %v", fault, err)
			}
			if _, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("refused SQL confirmation wrote history")
			}
		})
	}
}

func TestPolicyConfirmationConcurrentAnswersRetainOneUserResponse(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			answer := "yes"
			if i%2 != 0 {
				answer = "no"
			}
			_, err := s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", answer), allow)
			if err != nil && !errors.Is(err, ErrPolicyConfirmationConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var count int
	mustNil(t, s.db.QueryRow(`SELECT COUNT(*) FROM pipeline_policy_confirmations`).Scan(&count))
	got, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID)
	mustNil(t, err)
	if count != 1 || got.Revision != 2 {
		t.Fatalf("concurrent confirmation rows=%d revision=%d", count, got.Revision)
	}
	other := policyAnswer(candidate, "root", got.Answer)
	if _, err := s.AnswerPipelinePolicy(ctx, other, allow); !errors.Is(err, ErrPolicyConfirmationConflict) {
		t.Fatalf("another manager took credit for the first answer: %v", err)
	}
}
