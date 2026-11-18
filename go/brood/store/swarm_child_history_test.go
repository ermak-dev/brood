package store

import (
	"context"
	"errors"
	"testing"
)

// FEATURE: История детей доступна доверенному human adapter независимо от live actors.
// SPEC: Чтение не снимает holds, сохраняет старые UID/incarnation и фильтруется по swarm.
func TestSwarmChildHistorySurvivesOwnerStopAndActorReset(t *testing.T) {
	ctx := context.Background()
	s, p, executor := childFixture(t, "all", 2, 0)
	child := reserveChild(t, s, p, executor, "history-child")
	child, err := s.StartSwarmChild(ctx, p, child.RequestID, executor, child.Revision, childAllow)
	mustNil(t, err)
	_, err = s.StopSwarm(ctx, "one", "u", allow)
	mustNil(t, err)
	setActor(t, s, "a", "incarnation=2")
	setActor(t, s, "b", "incarnation=2")
	history, err := s.GetSwarmChildHistory(ctx, "one", p.Sequence)
	mustNil(t, err)
	if history.Parent != p || !history.Interrupted || history.Evaluation.Open != 2 || len(history.Children) != 1 || history.Children[0].Executor != executor || history.Children[0].State != "running" {
		t.Fatalf("history lost original scope or uncertain effects: %+v", history)
	}
	page, err := s.SwarmChildGroups(ctx, "one", 0, 25)
	mustNil(t, err)
	if len(page.Items) != 1 || !page.Items[0].Interrupted || page.Items[0].Registered != 1 || page.Items[0].Open != 2 || page.Items[0].Started != 1 {
		t.Fatalf("interrupted metadata lost outstanding work: %+v", page)
	}
	if _, err := s.GetSwarmChildJoin(ctx, p, childAllow); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("historical read relaxed guest authorization: %v", err)
	}
	if _, err := s.GetSwarmChildHistory(ctx, "two", p.Sequence); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign swarm aliases original parent sequence: %v", err)
	}
	if err := s.CheckActorChildObligations(ctx, p.Principal.ActorUID); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("reading history released automatic holds: %v", err)
	}
}

func TestSwarmChildHistoryRetainsAbortedPolicyAndPayload(t *testing.T) {
	ctx := context.Background()
	s, p, executor := childFixture(t, "all", 2, 0)
	child := reserveChild(t, s, p, executor, "unstarted")
	session := childRecoverySession(t, s, "u")
	aborted, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
	mustNil(t, err)
	history, err := s.GetSwarmChildHistory(ctx, "one", p.Sequence)
	mustNil(t, err)
	if history.State != "aborted" || history.AbortedBy != "u" || history.AbortedAt == "" || history.Policy != aborted.Policy || history.Policy.Total != 2 || len(history.Children) != 1 || history.Children[0].ID != child.ID || history.Children[0].Payload != child.Payload {
		t.Fatalf("aborted history lost evidence: %+v", history)
	}
	page, err := s.SwarmChildGroups(ctx, "one", 0, 25)
	mustNil(t, err)
	if len(page.Items) != 1 || page.Items[0].State != "aborted" || page.Items[0].Policy.Total != 2 || page.Items[0].Registered != 1 || page.Items[0].Open != 0 || page.Items[0].Started != 0 {
		t.Fatalf("aborted metadata invents active missing work: %+v", page)
	}
	for _, sequence := range []int64{0, -1, 99} {
		if _, err := s.GetSwarmChildHistory(ctx, "one", sequence); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown parent sequence %d: %v", sequence, err)
		}
	}
}

func TestSwarmChildHistoryPreservesCompletedReport(t *testing.T) {
	ctx := context.Background()
	s, p, executor := childFixture(t, "all", 1, 0)
	child := reserveChild(t, s, p, executor, "report")
	child, err := s.StartSwarmChild(ctx, p, child.RequestID, executor, child.Revision, childAllow)
	mustNil(t, err)
	const report = "Independent report: <script>not executable</script>\nResult verified."
	_, err = s.CompleteSwarmChild(ctx, p, child.RequestID, executor, child.Revision, "success", report, childAllow)
	mustNil(t, err)
	history, err := s.GetSwarmChildHistory(ctx, "one", p.Sequence)
	mustNil(t, err)
	if len(history.Children) != 1 || history.Children[0].Report != report || history.Children[0].Outcome != "success" || history.Children[0].State != "completed" || history.Children[0].FinishedAt == "" {
		t.Fatalf("history changed completed evidence: %+v", history.Children)
	}
}

func TestSwarmChildHistoryPaginationAndSummary(t *testing.T) {
	ctx := context.Background()
	s, p, executor := childFixture(t, "all", 1, 0)
	parents := []SwarmChildParent{p}
	for _, key := range []string{"second", "third"} {
		finishChild(t, s, p, executor, "completed-"+key, "success")
		_, err := s.JoinSwarmChildren(ctx, p, childAllow)
		mustNil(t, err)
		_, err = startOne(s, "build-first")
		mustNil(t, err)
		attempt := attestOne(t, s, "parent-"+key)
		mustNil(t, s.FinishActorOperation(ctx, attempt.Lease, true))
		p = SwarmChildParent{Principal: p.Principal, Pass: attempt.Pass, Sequence: attempt.Sequence, RequestID: attempt.RequestID}
		_, err = s.CreateSwarmChildJoin(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, childAllow)
		mustNil(t, err)
		parents = append(parents, p)
	}
	var before int64
	for index := len(parents) - 1; index >= 0; index-- {
		page, err := s.SwarmChildGroups(ctx, "one", before, 1)
		mustNil(t, err)
		if len(page.Items) != 1 || page.Items[0].Parent != parents[index] || page.Items[0].Policy.Total != 1 {
			t.Fatalf("history ordering or scope: %+v", page)
		}
		item := page.Items[0]
		if index == len(parents)-1 {
			if item.Registered != 0 || item.Open != 1 || item.Started != 0 {
				t.Fatalf("missing member summary is misleading: %+v", item)
			}
		} else if item.Registered != 1 || item.Open != 0 || item.Started != 1 {
			t.Fatalf("settled member summary is misleading: %+v", item)
		}
		if index == 0 {
			if page.NextBefore != 0 {
				t.Fatalf("last page advertises more history: %+v", page)
			}
		} else if page.NextBefore != item.Parent.Sequence {
			t.Fatalf("cursor does not pin last returned sequence: %+v", page)
		}
		before = page.NextBefore
	}
	foreign, err := s.SwarmChildGroups(ctx, "two", 0, 25)
	mustNil(t, err)
	if foreign.Items == nil || len(foreign.Items) != 0 || foreign.NextBefore != 0 {
		t.Fatalf("foreign swarm leaked group metadata: %+v", foreign)
	}
	for _, input := range []struct {
		before int64
		limit  int
	}{{-1, 25}, {0, 0}, {0, 101}} {
		if _, err := s.SwarmChildGroups(ctx, "one", input.before, input.limit); !errors.Is(err, ErrSwarmChildConflict) {
			t.Fatalf("invalid pagination accepted: %+v, %v", input, err)
		}
	}
}
