package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// BUG: Группа только из ephemeral children может обойти вычисление parent outcome (#292).
// HYPOTHESIS: checkChildParentResult распознаёт engine только по непустой workflow role.
// SOLUTION: Immutable ephemeral intents тоже требуют server-computed join authority.
// RESULT: FIXED — generic completion не может выдать иной aggregate или обойти join.
// STATUS: FIXED
func TestEphemeralQueuedParentCannotAcceptGenericResult(t *testing.T) {
	ctx := context.Background()
	s, intent := allocationFixture(t, 1)
	p := intent.Parent
	before := databaseRows(t, s.db)
	err := s.CompletePipelineActorVisit(ctx, p.Principal.ProjectID, p.Principal.SwarmID, p.Sequence, p.RequestID, p.Principal.ActorUID, p.Principal.Incarnation, "success", "Queued children cannot be skipped")
	if !errors.Is(err, ErrSwarmChildConflict) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatalf("queued child parent completion was accepted or changed evidence: %v", err)
	}
}

func TestLegacyAdHocChildrenKeepGenericCompletionContract(t *testing.T) {
	ctx := context.Background()
	s, parent, executor := childFixture(t, "all", 1, 0)
	finishChild(t, s, parent, executor, "legacy", "success")
	err := s.CompletePipelineActorVisit(ctx, parent.Principal.ProjectID, parent.Principal.SwarmID, parent.Sequence, parent.RequestID, parent.Principal.ActorUID, parent.Principal.Incarnation, "success", "Legacy parent report")
	mustNil(t, err)
	visit, err := s.GetSwarmStep(ctx, parent.Principal.SwarmID, parent.Sequence)
	mustNil(t, err)
	if visit.State != "completed" || visit.Report != "Legacy parent report" {
		t.Fatal("ephemeral join fence changed ordinary ad-hoc completion")
	}
}

func TestEphemeralChildrenRequireServerComputedParentResult(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact-report-without-join", true: "forged-success"}[forged], func(t *testing.T) {
			ctx := context.Background()
			s, intent, child, input := childDeliveryFixture(t)
			_, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, input, childAllow)
			mustNil(t, err)
			_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision+1, "fail", "Independent child failed", childAllow)
			mustNil(t, err)
			join, err := s.GetSwarmChildJoin(ctx, intent.Parent, childAllow)
			mustNil(t, err)
			if join.Evaluation.Open != 0 || join.Evaluation.State != "ready_fail" {
				t.Fatalf("fixture did not retain settled failed child: %+v", join)
			}
			outcome, report := "fail", ChildJoinReport(join)
			if forged {
				outcome, report = "success", "Forged parent success"
			}
			before := databaseRows(t, s.db)
			p := intent.Parent
			err = s.CompletePipelineActorVisit(ctx, p.Principal.ProjectID, p.Principal.SwarmID, p.Sequence, p.RequestID, p.Principal.ActorUID, p.Principal.Incarnation, outcome, report)
			if !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("ephemeral parent bypassed server join: %v", err)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refused parent completion mutated retained evidence")
			}
			_, err = s.JoinSwarmChildren(ctx, p, childAllow)
			mustNil(t, err)
			visit, err := s.GetSwarmStep(ctx, p.Principal.SwarmID, p.Sequence)
			mustNil(t, err)
			if visit.State != "completed" || visit.Outcome == nil || *visit.Outcome != "fail" || visit.Report != ChildJoinReport(join) {
				t.Fatalf("server join did not compute exact failed aggregate: %+v", visit)
			}
		})
	}
}
