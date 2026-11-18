package store

import (
	"context"
	"errors"
	"testing"
)

// BUG: Незавершённые дети запрещают владельцу остановить автоматический проход.
// SPEC: Остановка фиксируется отдельно от исходов детей; отчёты сохраняются,
// новые отправки и автоматическое продвижение остаются запрещены.
func TestOwnerStopsPassWithoutCancellingChildren(t *testing.T) {
	for _, state := range []string{"missing", "pending", "running"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, p, executor := childFixture(t, "all", 1, 0)
			var child SwarmChildTask
			if state != "missing" {
				child = reserveChild(t, s, p, executor, "retained-child")
			}
			if state == "running" {
				var err error
				child, err = s.StartSwarmChild(ctx, p, child.RequestID, executor, child.Revision, childAllow)
				mustNil(t, err)
			}
			sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
			mustNil(t, err)
			stopped, err := s.StopSwarm(ctx, sw.ID, sw.OwnerID, allow)
			mustNil(t, err)
			if stopped.Execution != "stopped" {
				t.Fatalf("owner stop did not freeze pass: %+v", stopped)
			}
			if _, err := startOne(s, "build-first"); !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("new automatic pass needs a typed outstanding-obligation refusal: %v", err)
			}
			join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if !join.Interrupted || join.Evaluation.State != "unconfirmed" {
				t.Fatalf("stop was mistaken for task completion: %+v", join)
			}
			if state != "missing" {
				if len(join.Children) != 1 || join.Children[0].State != state || join.Children[0].Outcome != "" || join.Children[0].FinishedAt != "" {
					t.Fatalf("stop cancelled or completed retained child: %+v", join.Children)
				}
			}
			if state == "pending" {
				if _, err := s.StartSwarmChild(ctx, p, child.RequestID, executor, child.Revision, childAllow); err == nil {
					t.Fatal("stopped pass admitted automatic child execution")
				}
			}
			if state == "running" {
				_, err := s.CompleteSwarmChild(ctx, p, child.RequestID, executor, child.Revision, "success", "late child evidence after pass stop", childAllow)
				mustNil(t, err)
				joined, err := s.JoinSwarmChildren(ctx, p, childAllow)
				mustNil(t, err)
				if !joined.Interrupted || joined.Children[0].Report != "late child evidence after pass stop" || stepOf(t, s, p.Sequence).State != "unconfirmed" {
					t.Fatalf("late evidence progressed stopped parent or was lost: %+v", joined)
				}
			}
			if err := s.CheckActorChildObligations(ctx, p.Principal.ActorUID); !errors.Is(err, ErrCoordinationConflict) {
				t.Fatalf("stop discarded outstanding automatic hold: %v", err)
			}
		})
	}
}

func TestPassStopChildOverrideRequiresOwnerOrAdmin(t *testing.T) {
	ctx := context.Background()
	s, p, _ := childFixture(t, "all", 1, 0)
	mustNil(t, s.SetSwarmMember(ctx, "one", "v", "editor"))
	if _, err := s.StopSwarm(ctx, "one", "v", allow); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("editor bypassed owner interruption policy: %v", err)
	}
	if interrupted, err := s.interruptedStep(ctx, "one", p.Sequence); err != nil || interrupted {
		t.Fatalf("refused editor stop journaled interruption: %v %v", interrupted, err)
	}
	stopped, err := s.StopSwarm(ctx, "one", "root", allow)
	mustNil(t, err)
	if stopped.Execution != "stopped" {
		t.Fatalf("administrator could not freeze pass: %+v", stopped)
	}
}

func TestPassStopWithoutCurrentSessionPreservesChildren(t *testing.T) {
	ctx := context.Background()
	s, p, _ := childFixture(t, "all", 1, 0)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	if _, err := s.StopSwarm(ctx, sw.ID, sw.OwnerID, func(context.Context) bool { return false }); err == nil {
		t.Fatal("expired session stopped pass")
	}
	after, err := s.GetSwarm(ctx, sw.ID)
	mustNil(t, err)
	if after.Execution != sw.Execution || stepOf(t, s, 1).State == "unconfirmed" {
		t.Fatal("refused stop mutated pass")
	}
	if _, err := s.db.Exec(`UPDATE swarms SET execution='stopped' WHERE id=?`, sw.ID); err == nil {
		t.Fatal("unaudited raw SQL stop bypassed retained child fence")
	}
}
