package store

// FEATURE: Explicit GitLab scopes and immutable identity after resource deletion.
// SPEC: A read grant never creates effects; master role is not authority; failed
// migration rolls back newly created tables; audit identity survives deletion.
// IMPLEMENTATION: Immutable records retain actor/human and credential-free binding snapshots.
// RESULT: IMPLEMENTED — scope denials, deletion/rotation retention and migration rollback verified.
// STATUS: DONE

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGitLabNarrowGrantAndMasterNotAuthority(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	setActor(t, s, p.ActorUID, "role='master',delegator_id='admin'")
	gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "read"); return e })
	g, err := s.CreateGitLabGrant(ctx, "u", GitLabGrant{Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour)})
	mustNil(t, err)
	got, err := s.CheckGitLabAccess(ctx, p, "read")
	mustNil(t, err)
	if got.ID != g.ID {
		t.Fatal("wrong grant")
	}
	gitlabDeniedUnchanged(t, s, func() error { _, e := gitlabReserve(s, p, "read-cannot-create"); return e })
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.ReserveGitLabProposal(ctx, p, "read-cannot-propose", gitlabSnapshot(p, b))
		return e
	})
}

func TestGitLabGrantCreationDenials(t *testing.T) {
	for _, change := range []string{"project", "swarm", "uid", "incarnation", "atespace", "revision", "empty-scope", "grantor", "expiry"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			g := GitLabGrant{Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour)}
			human := "u"
			switch change {
			case "project":
				g.Principal.ProjectID = "two"
			case "swarm":
				g.Principal.SwarmID = "two"
			case "uid":
				g.Principal.ActorUID = "other"
			case "incarnation":
				g.Principal.Incarnation++
			case "atespace":
				g.Principal.AtespaceUID = "other"
			case "revision":
				g.BindingRevision++
			case "empty-scope":
				g.Read = false
			case "grantor":
				human = "outsider"
			case "expiry":
				g.ExpiresAt = time.Now().Add(-time.Second)
			}
			gitlabDeniedUnchanged(t, s, func() error { _, e := s.CreateGitLabGrant(ctx, human, g); return e })
		})
	}
}

func TestGitLabApprovalInvalidProposalAndSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	gitlabGrant(t, s, p, b)
	for _, change := range []func(*GitLabMergeSnapshot){
		func(q *GitLabMergeSnapshot) { q.MRIID = 0 },
		func(q *GitLabMergeSnapshot) { q.PipelineID = 0 },
		func(q *GitLabMergeSnapshot) { q.SourceSHA = "not-sha" },
		func(q *GitLabMergeSnapshot) { q.PipelineSHA = strings.Repeat("b", 40) },
		func(q *GitLabMergeSnapshot) { q.TargetBranch = "foreign-target" },
		func(q *GitLabMergeSnapshot) { q.GitLabProjectID = 43 },
		func(q *GitLabMergeSnapshot) { q.SourceBranch = q.TargetBranch },
		func(q *GitLabMergeSnapshot) { q.Requester.ActorUID = "other" },
	} {
		q := gitlabSnapshot(p, b)
		change(&q)
		gitlabDeniedUnchanged(t, s, func() error { _, e := s.ReserveGitLabProposal(ctx, p, "invalid", q); return e })
	}
	op := gitlabProposal(t, s, p, b)
	expired := time.Now().Add(-time.Second)
	mustNil(t, s.CreateSession(ctx, Session{TokenHash: "expired", UserID: "u", ExpiresAt: &expired}))
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.ApproveGitLabMerge(ctx, op.ID, "expired", time.Now().Add(time.Hour), gitlabPolicy)
		return e
	})
}

func TestGitLabHistorySurvivesIdentityDeletion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	gitlabSession(t, s)
	proposal := gitlabProposal(t, s, p, b)
	_, err := s.ApproveGitLabMerge(ctx, proposal.ID, "approver-session", time.Now().Add(time.Hour), gitlabPolicy)
	mustNil(t, err)
	op, err := gitlabReserve(s, p, "outbound")
	mustNil(t, err)
	op, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending")
	mustNil(t, err)
	before := gitlabRows(t, s)
	// Delete the disposable actor first, as the existing swarm deletion guard requires.
	_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid='actor'; DELETE FROM users WHERE id='u'`)
	mustNil(t, err)
	if after := gitlabRows(t, s); !reflect.DeepEqual(before, after) {
		t.Fatal("identity deletion erased GitLab history")
	}
	_, err = s.GetGitLabGrant(ctx, g.ID)
	mustNil(t, err)
	got, err := s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "unknown")
	mustNil(t, err)
	if got.Principal != p || got.GrantID != g.ID {
		t.Fatal("uncertain effect lost its identity")
	}
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.ClaimGitLabApproval(ctx, proposal.ID, 1, gitlabSnapshot(p, b), gitlabPolicy)
		return e
	})
}

func TestGitLabJournalCannotReplaceScopedRequest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	gitlabGrant(t, s, p, b)
	op, err := gitlabReserve(s, p, "immutable-request")
	mustNil(t, err)
	op, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending")
	mustNil(t, err)
	gitlabDeniedUnchanged(t, s, func() error {
		_, err := s.db.Exec(`INSERT OR REPLACE INTO gitlab_operations(id,project_id,swarm_id,atespace_uid,actor_uid,incarnation,kind,request_id,grant_id,binding_revision,binding_snapshot,payload_hash,snapshot,state,revision,created_at,updated_at)
  SELECT 'replacement',project_id,swarm_id,atespace_uid,actor_uid,incarnation,kind,request_id,grant_id,binding_revision,binding_snapshot,payload_hash,snapshot,'reserved',1,created_at,updated_at FROM gitlab_operations WHERE id=?`, op.ID)
		return err
	})
}

func TestGitLabEffectRetainsUpstreamBindingOnRotation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	gitlabGrant(t, s, p, b)
	op, err := gitlabReserve(s, p, "uncertain")
	mustNil(t, err)
	op, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending")
	mustNil(t, err)
	rotated := b
	rotated.GitLabProjectID = 77
	rotated.CredentialRef = "rotated-api"
	rotated.TargetBranch = "release"
	_, err = s.PutGitLabBinding(ctx, "admin", rotated, b.Revision)
	mustNil(t, err)
	op, err = s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "unknown")
	mustNil(t, err)
	if op.Binding != b {
		t.Fatalf("rotation lost the upstream target of a sent effect: %+v", op.Binding)
	}
	gitlabDeniedUnchanged(t, s, func() error {
		_, e := s.db.Exec(`UPDATE gitlab_operations SET binding_snapshot='{}' WHERE id=?`, op.ID)
		return e
	})
}

func TestGitLabMigrationFailureRollsBack32(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 32)
	raw := rawDatabase(t, path)
	_, err := raw.Exec(`CREATE TABLE gitlab_grants (collision TEXT)`)
	mustNil(t, err)
	objects, rows := schemaObjects(t, raw), databaseRows(t, raw)
	raw.Close()
	s, err := Open(ctx, path)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("conflicting table migration succeeded")
	}
	raw = rawDatabase(t, path)
	defer raw.Close()
	var version int
	mustNil(t, raw.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != 32 || !reflect.DeepEqual(objects, schemaObjects(t, raw)) || !reflect.DeepEqual(rows, databaseRows(t, raw)) {
		t.Fatal("failed 32->33 migration changed schema, rows, or version")
	}
}
