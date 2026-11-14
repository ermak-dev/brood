package store

// FEATURE: Explicit GitLab authority, durable effects, and single-use merge snapshots.
// SPEC: Default deny; current identity and rights in transactions; immutable intent,
// CAS, no replay after sending, and no merge authority from generic answers.
// IMPLEMENTATION: Schema 33 stores explicit bindings/grants, immutable effect
// identity and typed proposals; transactional policy and CAS consume approvals.
// RESULT: IMPLEMENTED — SQLite denial/rollback, restart, revocation and concurrency verified.
// STATUS: DONE

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func gitlabFixture(t *testing.T, s *Store) (GitLabPrincipal, GitLabBinding) {
	t.Helper()
	ctx := context.Background()
	managedFixture(t, s)
	mustNil(t, s.AddMember(ctx, "one", "u"))
	seedUser(t, s, "admin", "gitlab-admin")
	mustNil(t, s.SetUserAdmin(ctx, "admin", true))
	mustNil(t, s.SetProjectRepository(ctx, "one", "https://git.example/group/repo.git"))
	a, err := s.ReserveManagedActor(ctx, managedRequest("actor", "one", 101))
	mustNil(t, err)
	a.Phase = "running"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "running")
	setActor(t, s, a.UID, settledRunning)
	p := GitLabPrincipal{ProjectID: "one", SwarmID: "one", AtespaceUID: "native-one", ActorUID: a.UID, Incarnation: a.Incarnation}
	b, err := s.PutGitLabBinding(ctx, "admin", GitLabBinding{ProjectID: "one", Repository: "https://git.example/group/repo.git", APIOrigin: "https://git.example", GitLabProjectID: 42, TargetBranch: "main", CredentialRef: "gitlab-api", Enabled: true}, 0)
	mustNil(t, err)
	return p, b
}
func gitlabGrant(t *testing.T, s *Store, p GitLabPrincipal, b GitLabBinding) GitLabGrant {
	t.Helper()
	g, err := s.CreateGitLabGrant(context.Background(), "u", GitLabGrant{Principal: p, BindingRevision: b.Revision, Read: true, Create: true, Propose: true, ExpiresAt: time.Now().Add(time.Hour)})
	mustNil(t, err)
	return g
}
func gitlabRows(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, name := range []string{"gitlab_bindings", "gitlab_grants", "gitlab_operations", "gitlab_approvals"} {
		out[name] = tableRows(t, s.db, name)
	}
	return out
}
func gitlabDeniedUnchanged(t *testing.T, s *Store, fn func() error) {
	t.Helper()
	before := gitlabRows(t, s)
	if err := fn(); err == nil {
		t.Fatal("denied operation succeeded")
	}
	if after := gitlabRows(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("denial changed authority or intent: before=%v after=%v", before, after)
	}
}
func gitlabReserve(s *Store, p GitLabPrincipal, id string) (GitLabOperation, error) {
	return s.ReserveGitLabMutation(context.Background(), p, "create", id, []byte(`{"source":"feature"}`))
}
func TestGitLabDefaultDenyAndBindingValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	gitlabDeniedUnchanged(t, s, func() error { _, e := gitlabReserve(s, p, "no-grant"); return e })
	for _, change := range []func(*GitLabBinding){
		func(b *GitLabBinding) { b.Repository = "https://git.example/other/repo" },
		func(b *GitLabBinding) { b.APIOrigin = "https://evil.example" },
		func(b *GitLabBinding) { b.APIOrigin = "https://git.example/api/v4" },
		func(b *GitLabBinding) { b.APIOrigin = "https://token@git.example" },
		func(b *GitLabBinding) { b.APIOrigin = "http://git.example" },
		func(b *GitLabBinding) { b.GitLabProjectID = 0 },
		func(b *GitLabBinding) { b.TargetBranch = "" },
		func(b *GitLabBinding) { b.CredentialRef = "glpat-secret bytes" },
	} {
		invalid := b
		change(&invalid)
		gitlabDeniedUnchanged(t, s, func() error { _, e := s.PutGitLabBinding(ctx, "admin", invalid, b.Revision); return e })
	}
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.PutGitLabBinding(ctx, "u", b, b.Revision); return e })
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.PutGitLabBinding(ctx, "admin", b, 0); return e })
	g := gitlabGrant(t, s, p, b)
	if g.HumanID != "u" || g.ID == "" {
		t.Fatalf("grant identity: %+v", g)
	}
	_, err := s.CheckGitLabAccess(ctx, p, "read")
	mustNil(t, err)
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "merge"); return e })
}
func TestGitLabGrantCurrentIdentityAndRights(t *testing.T) {
	for _, change := range []string{"incarnation", "actor", "atespace", "swarm", "project", "disabled", "membership", "swarm-role", "binding", "repository", "expired", "revoked", "actor-stopped", "swarm-inactive", "grantor-disabled"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			g := gitlabGrant(t, s, p, b)
			switch change {
			case "incarnation":
				p.Incarnation++
			case "actor":
				p.ActorUID = "foreign"
			case "atespace":
				p.AtespaceUID = "replaced"
			case "swarm":
				p.SwarmID = "two"
			case "project":
				p.ProjectID = "two"
			case "disabled", "grantor-disabled":
				_, err := s.db.Exec(`UPDATE users SET disabled=1 WHERE id='u'`)
				mustNil(t, err)
			case "membership":
				_, err := s.db.Exec(`DELETE FROM project_members WHERE user_id='u'`)
				mustNil(t, err)
			case "swarm-role":
				_, err := s.db.Exec(`UPDATE swarm_members SET role='viewer' WHERE user_id='u'`)
				mustNil(t, err)
			case "binding":
				b.Enabled = false
				_, err := s.PutGitLabBinding(ctx, "admin", b, b.Revision)
				mustNil(t, err)
			case "repository":
				mustNil(t, s.SetProjectRepository(ctx, "one", "https://git.example/group/other.git"))
			case "expired":
				_, err := s.db.Exec(`UPDATE gitlab_grants SET expires_at=1 WHERE id=?`, g.ID)
				if err == nil {
					t.Fatal("grant expiry was mutable")
				}
				g2 := GitLabGrant{Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(-time.Second)}
				gitlabDeniedUnchanged(t, s, func() error { _, e := s.CreateGitLabGrant(ctx, "u", g2); return e })
				mustNil(t, s.RevokeGitLabGrant(ctx, "u", g.ID, g.Revision))
			case "revoked":
				mustNil(t, s.RevokeGitLabGrant(ctx, "u", g.ID, g.Revision))
			case "actor-stopped":
				_, err := s.db.Exec(`UPDATE managed_actors SET phase='stopped',desired='stopped' WHERE uid='actor'`)
				mustNil(t, err)
			case "swarm-inactive":
				_, err := s.db.Exec(`UPDATE swarms SET state='error' WHERE id='one'`)
				mustNil(t, err)
			}
			gitlabDeniedUnchanged(t, s, func() error { _, e := gitlabReserve(s, p, "denied"); return e })
		})
	}
}
func TestGitLabJournalCASRollbackPayloadAndRevocation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	op, err := gitlabReserve(s, p, "create-1")
	mustNil(t, err)
	if op.State != "reserved" || op.Revision != 1 || len(op.PayloadHash) != 64 {
		t.Fatalf("intent: %+v", op)
	}
	retry, err := gitlabReserve(s, p, "create-1")
	mustNil(t, err)
	if retry != op {
		t.Fatalf("retry changed intent: %+v", retry)
	}
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.ReserveGitLabMutation(ctx, p, "create", "create-1", []byte(`{"source":"other"}`))
		return e
	})
	aborted := errors.New("abort")
	err = s.WithTx(ctx, func(ctx context.Context) error {
		_, e := s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending")
		if e != nil {
			return e
		}
		return aborted
	})
	if !errors.Is(err, aborted) {
		t.Fatal(err)
	}
	got, err := s.GetGitLabOperation(ctx, op.ID)
	mustNil(t, err)
	if got != op {
		t.Fatal("transaction did not roll back sending")
	}
	op, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending")
	mustNil(t, err)
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.TransitionGitLabOperation(ctx, op.ID, 1, "completed"); return e })
	mustNil(t, s.RevokeGitLabGrant(ctx, "u", g.ID, g.Revision))
	// Outcome recording is independent of current authority: revocation cannot erase a sent effect.
	op, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "unknown")
	mustNil(t, err)
	if op.Principal != p || op.GrantID != g.ID || op.State != "unknown" {
		t.Fatalf("lost effect identity: %+v", op)
	}
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending"); return e })
	gitlabDeniedUnchanged(t, s, func() error { _, e := gitlabReserve(s, p, "create-1"); return e })
}
func TestGitLabExpiredGrantAtUse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	_, err := s.CreateGitLabGrant(ctx, "u", GitLabGrant{Principal: p, BindingRevision: b.Revision, Create: true, ExpiresAt: time.Now().Add(80 * time.Millisecond)})
	mustNil(t, err)
	// An actual deadline, not a rewrite of the grant predicate or of the clock.
	<-time.After(100 * time.Millisecond)
	gitlabDeniedUnchanged(t, s, func() error { _, e := gitlabReserve(s, p, "expired"); return e })
}
func TestGitLabJournalConcurrentRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitlab.sqlite")
	s, err := Open(ctx, path)
	mustNil(t, err)
	p, b := gitlabFixture(t, s)
	gitlabGrant(t, s, p, b)
	other, err := Open(ctx, path)
	mustNil(t, err)
	defer other.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	ids := make(chan string, 12)
	for i := range 12 {
		wg.Go(func() {
			db := s
			if i%2 == 1 {
				db = other
			}
			op, e := gitlabReserve(db, p, "duplicate")
			errs <- e
			ids <- op.ID
		})
	}
	wg.Wait()
	close(errs)
	close(ids)
	for e := range errs {
		mustNil(t, e)
	}
	id := ""
	for got := range ids {
		if id != "" && got != id {
			t.Fatal("duplicate intents")
		}
		id = got
	}
	op, err := s.GetGitLabOperation(ctx, id)
	mustNil(t, err)
	winners := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			db := s
			if i%2 == 1 {
				db = other
			}
			_, e := db.TransitionGitLabOperation(ctx, id, op.Revision, "sending")
			winners <- e
		})
	}
	wg.Wait()
	close(winners)
	n := 0
	for e := range winners {
		if e == nil {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("send claims=%d", n)
	}
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	mustNil(t, s.RecoverGitLabOperations(ctx))
	got, err := gitlabReserve(s, p, "duplicate")
	mustNil(t, err)
	if got.ID != id || got.State != "unknown" || got.Principal != p {
		t.Fatalf("restart lost uncertain effect: %+v", got)
	}
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.TransitionGitLabOperation(ctx, id, got.Revision, "sending"); return e })
}
func gitlabSnapshot(p GitLabPrincipal, b GitLabBinding) GitLabMergeSnapshot {
	return GitLabMergeSnapshot{Requester: p, BindingRevision: b.Revision, GitLabProjectID: 42, MRIID: 12, SourceBranch: "feature", SourceSHA: strings.Repeat("a", 40), PipelineID: 99, PipelineSHA: strings.Repeat("a", 40), TargetBranch: "main"}
}
func gitlabPolicy(context.Context, GitLabApprovalContext) error { return nil }
func gitlabProposal(t *testing.T, s *Store, p GitLabPrincipal, b GitLabBinding) GitLabOperation {
	t.Helper()
	op, err := s.ReserveGitLabProposal(context.Background(), p, "proposal", gitlabSnapshot(p, b))
	mustNil(t, err)
	return op
}
func gitlabSession(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(time.Hour)
	mustNil(t, s.CreateSession(context.Background(), Session{TokenHash: "approver-session", UserID: "u", ExpiresAt: &deadline}))
}
func TestGitLabApprovalExactPolicySessionAndSingleUse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	gitlabGrant(t, s, p, b)
	gitlabSession(t, s)
	op := gitlabProposal(t, s, p, b)
	snapshot := gitlabSnapshot(p, b)
	deadline := time.Now().Add(time.Hour)
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", deadline, nil); return e })
	denied := func(context.Context, GitLabApprovalContext) error { return errors.New("policy denied") }
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", deadline, denied); return e })
	approval, err := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", deadline, gitlabPolicy)
	mustNil(t, err)
	for _, change := range []func(*GitLabMergeSnapshot){
		func(q *GitLabMergeSnapshot) { q.SourceSHA = strings.Repeat("b", 40) },
		func(q *GitLabMergeSnapshot) { q.PipelineID++ },
		func(q *GitLabMergeSnapshot) { q.TargetBranch = "release" },
		func(q *GitLabMergeSnapshot) { q.BindingRevision++ },
		func(q *GitLabMergeSnapshot) { q.Requester.Incarnation++ },
		func(q *GitLabMergeSnapshot) { q.GitLabProjectID++ },
		func(q *GitLabMergeSnapshot) { q.Squash = true },
	} {
		q := snapshot
		change(&q)
		gitlabDeniedUnchanged(t, s, func() error { _, e := s.ClaimGitLabApproval(ctx, op.ID, approval.Revision, q, gitlabPolicy); return e })
	}
	claimed, err := s.ClaimGitLabApproval(ctx, op.ID, approval.Revision, snapshot, gitlabPolicy)
	mustNil(t, err)
	if claimed.State != "claimed" || claimed.HumanID != "u" {
		t.Fatalf("claim %+v", claimed)
	}
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.ClaimGitLabApproval(ctx, op.ID, claimed.Revision, snapshot, gitlabPolicy)
		return e
	})
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", deadline, gitlabPolicy)
		if !errors.Is(e, ErrGitLabConflict) {
			t.Fatalf("duplicate approval must be a typed conflict: %v", e)
		}
		return e
	})
}
func TestGitLabApprovalLateRevocation(t *testing.T) {
	for _, change := range []string{"session", "user", "role", "grant", "binding", "approval", "expired", "policy"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			g := gitlabGrant(t, s, p, b)
			gitlabSession(t, s)
			op := gitlabProposal(t, s, p, b)
			deadline := time.Now().Add(time.Hour)
			if change == "expired" {
				deadline = time.Now().Add(80 * time.Millisecond)
			}
			a, err := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", deadline, gitlabPolicy)
			mustNil(t, err)
			policy := gitlabPolicy
			switch change {
			case "session":
				mustNil(t, s.DeleteSession(ctx, "approver-session"))
			case "user":
				_, err = s.db.Exec(`UPDATE users SET disabled=1 WHERE id='u'`)
				mustNil(t, err)
			case "role":
				_, err = s.db.Exec(`UPDATE swarm_members SET role='viewer' WHERE user_id='u'`)
				mustNil(t, err)
			case "grant":
				mustNil(t, s.RevokeGitLabGrant(ctx, "u", g.ID, g.Revision))
			case "binding":
				_, err = s.PutGitLabBinding(ctx, "admin", b, b.Revision)
				mustNil(t, err)
			case "approval":
				mustNil(t, s.RevokeGitLabApproval(ctx, "u", op.ID, a.Revision))
			case "expired":
				<-time.After(100 * time.Millisecond)
			case "policy":
				policy = nil
			}
			gitlabDeniedUnchanged(t, s, func() error {
				_, e := s.ClaimGitLabApproval(ctx, op.ID, a.Revision, gitlabSnapshot(p, b), policy)
				return e
			})
		})
	}
}
func TestGitLabImmutableRowsAndNoAuthorityFromAnswer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	gitlabSession(t, s)
	op := gitlabProposal(t, s, p, b)
	_, err := s.ApproveGitLabMerge(ctx, op.ID, "approver-session", time.Now().Add(time.Hour), gitlabPolicy)
	mustNil(t, err)
	for _, query := range []string{
		fmt.Sprintf(`UPDATE gitlab_grants SET human_id='admin' WHERE id='%s'`, g.ID),
		`UPDATE gitlab_operations SET payload_hash='other'`,
		`UPDATE gitlab_operations SET snapshot='{}'`,
		`UPDATE gitlab_approvals SET session_hash='other'`,
		`DELETE FROM gitlab_grants`, `DELETE FROM gitlab_operations`, `DELETE FROM gitlab_approvals`,
	} {
		gitlabDeniedUnchanged(t, s, func() error { _, e := s.db.Exec(query); return e })
	}
	// Даже при существующем proposal в той же базе ответ на generic request
	// не меняет GitLab authority и не становится решением по этому proposal.
	requestDB, node := requestStore(t)
	actorRequest(t, requestDB, "text", nil)
	setActor(t, requestDB, "worker", settledRunning)
	mustNil(t, requestDB.SetProjectRepository(ctx, "p", b.Repository))
	bound := b
	bound.ProjectID = "p"
	bound, err = requestDB.PutGitLabBinding(ctx, "adm", bound, 0)
	mustNil(t, err)
	requester := GitLabPrincipal{ProjectID: "p", SwarmID: "s", AtespaceUID: "native-s", ActorUID: "worker", Incarnation: 1}
	gitlabGrant(t, requestDB, requester, bound)
	proposal := gitlabProposal(t, requestDB, requester, bound)
	before := gitlabRows(t, requestDB)
	mustNil(t, answer(requestDB, node, "u", "success", "approve merge MR 12"))
	if !reflect.DeepEqual(before, gitlabRows(t, requestDB)) {
		t.Fatal("generic answer changed GitLab authority")
	}
	if _, err := requestDB.GetGitLabApproval(ctx, proposal.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("generic answer created a merge decision: %v", err)
	}
}
