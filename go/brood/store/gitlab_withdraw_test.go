package store

// BUG: Одобривший editor не может отозвать собственное решение.
// HYPOTHESIS: Отзыв всегда требует owner вместо проверки исходного approver.
// SOLUTION: Для собственного решения проверяются исходная сессия и текущие права.
// RESULT: FIXED — чужой editor, отключение, потеря сессии и CAS не обходятся.
// STATUS: FIXED

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGitLabEditorWithdrawOwnApproval(t *testing.T) {
	for _, mode := range []string{"own", "other-editor", "expired-session", "disabled", "claimed", "wrong-revision", "owner"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			gitlabGrant(t, s, p, b)
			seedUser(t, s, "editor", "withdraw-editor")
			mustNil(t, s.AddMember(ctx, "one", "editor"))
			mustNil(t, s.SetSwarmMember(ctx, "one", "editor", "editor"))
			mustNil(t, s.CreateSession(ctx, Session{TokenHash: "editor-session", UserID: "editor"}))
			op, err := s.ReserveGitLabProposal(ctx, p, "withdraw", gitlabSnapshot(p, b))
			mustNil(t, err)
			a, err := s.ApproveGitLabMerge(ctx, op.ID, "editor-session", time.Now().Add(time.Minute), gitlabPolicy)
			mustNil(t, err)
			human := "editor"
			revision := a.Revision
			switch mode {
			case "other-editor":
				seedUser(t, s, "other", "withdraw-other")
				mustNil(t, s.AddMember(ctx, "one", "other"))
				mustNil(t, s.SetSwarmMember(ctx, "one", "other", "editor"))
				human = "other"
			case "expired-session":
				_, err = s.DeleteSessionsOfUser(ctx, "editor")
				mustNil(t, err)
			case "disabled":
				_, err = s.db.Exec(`UPDATE users SET disabled=1 WHERE id='editor'`)
				mustNil(t, err)
			case "claimed":
				_, err = s.ClaimGitLabApproval(ctx, op.ID, a.Revision, op.Snapshot, gitlabPolicy)
				mustNil(t, err)
			case "wrong-revision":
				revision++
			case "owner":
				human = "u"
			}
			err = s.RevokeGitLabApproval(ctx, human, op.ID, revision)
			if mode == "own" || mode == "owner" {
				mustNil(t, err)
				got, e := s.GetGitLabApproval(ctx, op.ID)
				mustNil(t, e)
				if got.State != "revoked" || got.HumanID != "editor" || got.SessionHash != "editor-session" {
					t.Fatalf("withdrawal lost history: %+v", got)
				}
			} else if !errors.Is(err, ErrGitLabDenied) && !errors.Is(err, ErrGitLabConflict) {
				t.Fatalf("unauthorized withdrawal: %v", err)
			}
		})
	}
}
