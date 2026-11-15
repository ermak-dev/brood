package store

// FEATURE: Pinned proposals and irreversible typed decision state in SQLite.
// SPEC: No grant substitution; rejected/stale proposals cannot be approved or
// consumed; an approval expiry cannot exceed its proposal or delegated grant.
// IMPLEMENTATION: Schema 34 adds terminal proposal state and rejection audit;
// existing immutable approvals remain exact-snapshot, expiring and single-use.
// RESULT: IMPLEMENTED — exact retries, denial, audit, expiry and schema migration pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGitLabProposalRejectionAuditAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	snapshot := gitlabSnapshot(p, b)
	snapshot.Evidence = `{"jobs":[{"id":1,"status":"success"}]}`
	snapshot.ExpiresAt = g.ExpiresAt.UnixNano()
	snapshot.TargetSHA = snapshot.SourceSHA
	op, err := s.ReserveGitLabProposalWithGrant(ctx, p, "audit", g.ID, snapshot)
	mustNil(t, err)
	gitlabSession(t, s)
	_, err = s.ApproveGitLabMerge(ctx, op.ID, "approver-session", g.ExpiresAt.Add(time.Second), gitlabPolicy)
	if !errors.Is(err, ErrGitLabDenied) {
		t.Fatalf("approval outlived proposal: %v", err)
	}
	mustNil(t, s.DecideGitLabProposal(ctx, op.ID, "approver-session", op.PayloadHash, "reject", op.Revision, time.Now().Add(time.Minute), gitlabPolicy))
	var human, session string
	mustNil(t, s.db.QueryRow(`SELECT human_id,session_hash FROM gitlab_proposal_states WHERE operation_id=?`, op.ID).Scan(&human, &session))
	if human != "u" || session != "approver-session" {
		t.Fatalf("lost rejecting identity: %q %q", human, session)
	}
	_, err = s.ApproveGitLabMerge(ctx, op.ID, "approver-session", time.Now().Add(time.Minute), gitlabPolicy)
	if !errors.Is(err, ErrGitLabDenied) {
		t.Fatalf("rejected proposal approved: %v", err)
	}
}

func TestGitLabPinnedProposalState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	snapshot := gitlabSnapshot(p, b)
	snapshot.Evidence = `{"jobs":[{"id":1,"status":"success"}]}`
	snapshot.ExpiresAt = g.ExpiresAt.UnixNano()
	snapshot.TargetSHA = snapshot.SourceSHA
	op, err := s.ReserveGitLabProposalWithGrant(ctx, p, "proposal", g.ID, snapshot)
	mustNil(t, err)
	retry, err := s.ReserveGitLabProposalWithGrant(ctx, p, "proposal", g.ID, snapshot)
	mustNil(t, err)
	if retry != op {
		t.Fatal("retry changed immutable snapshot")
	}
	g2 := gitlabGrant(t, s, p, b)
	_, err = s.ReserveGitLabProposalWithGrant(ctx, p, "proposal", g2.ID, snapshot)
	if !errors.Is(err, ErrGitLabConflict) {
		t.Fatalf("grant substitution: %v", err)
	}
	mustNil(t, s.StaleGitLabProposal(ctx, op.ID))
	state, err := s.GitLabProposalState(ctx, op.ID)
	mustNil(t, err)
	if state != "stale" {
		t.Fatalf("state=%s", state)
	}
	gitlabSession(t, s)
	_, err = s.ApproveGitLabMerge(ctx, op.ID, "approver-session", time.Now().Add(time.Minute), func(context.Context, GitLabApprovalContext) error { return nil })
	if !errors.Is(err, ErrGitLabDenied) {
		t.Fatalf("stale approved: %v", err)
	}
	if _, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending"); err == nil {
		t.Fatal("proposal entered executor")
	}
}
