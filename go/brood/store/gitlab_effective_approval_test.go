package store

// BUG: Исторический approved не доказывает текущие права approver.
// HYPOTHESIS: Чтение approval не выполняет проверки потребления.
// SOLUTION: Non-consuming проверка повторяет current identity и policy в tx.
// RESULT: FIXED — проверка не меняет историю и отказывает потерянным правам.
// STATUS: FIXED

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGitLabEffectiveApprovalDoesNotClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	gitlabGrant(t, s, p, b)
	gitlabSession(t, s)
	op, err := s.ReserveGitLabProposal(ctx, p, "effective", gitlabSnapshot(p, b))
	mustNil(t, err)
	a, err := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", time.Now().Add(time.Minute), gitlabPolicy)
	mustNil(t, err)
	mustNil(t, s.CheckGitLabApproval(ctx, op.ID, gitlabPolicy))
	got, err := s.GetGitLabApproval(ctx, op.ID)
	mustNil(t, err)
	if got != a {
		t.Fatal("inspection consumed or changed approval")
	}
	if err = s.CheckGitLabApproval(ctx, op.ID, nil); !errors.Is(err, ErrGitLabDenied) {
		t.Fatalf("nil policy authorized: %v", err)
	}
	_, err = s.db.Exec(`UPDATE swarm_members SET role='viewer' WHERE swarm_id='one' AND user_id='u'`)
	mustNil(t, err)
	if err = s.CheckGitLabApproval(ctx, op.ID, gitlabPolicy); !errors.Is(err, ErrGitLabDenied) {
		t.Fatalf("lost rights authorized: %v", err)
	}
}
