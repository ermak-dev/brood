package store

// FEATURE: Transactional late GitLab authorization and durable effect intent.
// SPEC: A policy result cannot outlive its session or authorize changed identity;
// failed persistence never releases a sent effect, and a new grant cannot replay it.
// IMPLEMENTATION: Authority is checked before and after the explicit policy callback.
// RESULT: IMPLEMENTED — actual SQLite rollback, failed settlement, and two-connection claim verified.
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/gitcredential"
)

func TestGitLabPolicyLateAuthorityRollback(t *testing.T) {
	for _, stage := range []string{"approve", "claim"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			gitlabGrant(t, s, p, b)
			gitlabSession(t, s)
			op := gitlabProposal(t, s, p, b)
			revision := int64(0)
			if stage == "claim" {
				a, err := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", time.Now().Add(time.Hour), gitlabPolicy)
				mustNil(t, err)
				revision = a.Revision
			}
			before := gitlabRows(t, s)
			policy := func(ctx context.Context, _ GitLabApprovalContext) error {
				return s.DeleteSession(ctx, "approver-session")
			}
			var err error
			if stage == "approve" {
				_, err = s.ApproveGitLabMerge(ctx, op.ID, "approver-session", time.Now().Add(time.Hour), policy)
			} else {
				_, err = s.ClaimGitLabApproval(ctx, op.ID, revision, gitlabSnapshot(p, b), policy)
			}
			if err == nil {
				t.Fatal("policy revoked its session but approval still succeeded")
			}
			if !reflect.DeepEqual(before, gitlabRows(t, s)) {
				t.Fatal("late denial changed authority")
			}
			if _, err := s.GetSession(ctx, "approver-session"); err != nil {
				t.Fatal("failed approval did not roll back policy transaction")
			}
		})
	}
}

func TestGitLabIntentPersistenceFailures(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	before := gitlabRows(t, s)
	abort := errors.New("abort after reservation")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		_, e := s.ReserveGitLabMutation(ctx, p, "create", "rolled-back", []byte("payload"))
		if e != nil {
			return e
		}
		return abort
	})
	if !errors.Is(err, abort) || !reflect.DeepEqual(before, gitlabRows(t, s)) {
		t.Fatalf("reservation rollback: %v", err)
	}
	op, err := gitlabReserve(s, p, "send")
	mustNil(t, err)
	mustNil(t, s.RevokeGitLabGrant(ctx, "u", g.ID, g.Revision))
	gitlabGrant(t, s, p, b)
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending"); return e })
	gitlabDeniedUnchanged(t, s, func() error { _, e := gitlabReserve(s, p, "send"); return e })
	op, err = gitlabReserve(s, p, "second")
	mustNil(t, err)
	op, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending")
	mustNil(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER fail_gitlab_outcome BEFORE UPDATE ON gitlab_operations WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'disk failure'); END`)
	mustNil(t, err)
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "completed"); return e })
	got, err := s.GetGitLabOperation(ctx, op.ID)
	mustNil(t, err)
	if got.State != "sending" || got.Principal != p {
		t.Fatalf("failed settlement lost effect: %+v", got)
	}
	mustNil(t, s.RecoverGitLabOperations(ctx))
	got, err = gitlabReserve(s, p, "second")
	mustNil(t, err)
	if got.State != "unknown" {
		t.Fatal("failed settlement replayable")
	}
}

func TestGitLabTransportCredentialDoesNotEnableAPI(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	// Explicit binding disabled; the valid active transport credential must not be consulted.
	b.Enabled = false
	_, err := s.PutGitLabBinding(ctx, "admin", b, b.Revision)
	mustNil(t, err)
	mustNil(t, s.PutProjectGitCredential(ctx, "one", gitcredential.Credential{Name: "transport", Repository: b.Repository, Username: "git", Token: "transport-only-sentinel"}))
	mustNil(t, s.SetProjectGitCredentialRef(ctx, "one", "transport"))
	access, err := s.ProjectGitAccess(ctx, "one")
	mustNil(t, err)
	if !access.HasCredential() {
		t.Fatal("transport precondition absent")
	}
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "read"); return e })
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.CreateGitLabGrant(ctx, "u", GitLabGrant{Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour)})
		return e
	})
}

func TestGitLabApprovalConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/approval.sqlite"
	s, err := Open(ctx, path)
	mustNil(t, err)
	p, b := gitlabFixture(t, s)
	gitlabGrant(t, s, p, b)
	gitlabSession(t, s)
	op := gitlabProposal(t, s, p, b)
	a, err := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", time.Now().Add(time.Hour), gitlabPolicy)
	mustNil(t, err)
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	other, err := Open(ctx, path)
	mustNil(t, err)
	defer other.Close()
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			db := s
			if i%2 == 1 {
				db = other
			}
			_, e := db.ClaimGitLabApproval(ctx, op.ID, a.Revision, gitlabSnapshot(p, b), gitlabPolicy)
			results <- e
		})
	}
	wg.Wait()
	close(results)
	winners := 0
	for e := range results {
		if e == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("approval consumers=%d", winners)
	}
}
