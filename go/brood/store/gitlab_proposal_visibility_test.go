package store

// BUG: Сто чужих новых proposal скрывают старое доступное решение.
// HYPOTHESIS: LIMIT применяется до проверки swarm membership.
// SOLUTION: Текущая SQL-видимость проверяется до LIMIT; session остаётся в adapter.
// RESULT: FIXED — 105 чужих записей не скрывают доступную.
// STATUS: FIXED

import (
	"context"
	"fmt"
	"testing"
)

func TestGitLabProposalVisibilityBeforeLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	snapshot := gitlabSnapshot(p, b)
	snapshot.TargetSHA = snapshot.SourceSHA
	snapshot.Evidence = `{"jobs":[]}`
	snapshot.ExpiresAt = g.ExpiresAt.UnixNano()
	op, err := s.ReserveGitLabProposalWithGrant(ctx, p, "visible-old", g.ID, snapshot)
	mustNil(t, err)
	seedUser(t, s, "viewer", "proposal-viewer")
	mustNil(t, s.AddMember(ctx, "one", "viewer"))
	mustNil(t, s.SetSwarmMember(ctx, "one", "viewer", "viewer"))
	_, err = s.db.Exec(`UPDATE swarms SET project_id='one' WHERE id='two'`)
	mustNil(t, err)
	// Реальная SQLite-история другого роя того же проекта. Не создаём grants
	// с чужой authority: копии используются только как недоступные записи чтения.
	for i := range 105 {
		id := fmt.Sprintf("hidden-%03d", i)
		_, err = s.db.Exec(`INSERT INTO gitlab_operations SELECT ?,project_id,'two',atespace_uid,actor_uid,incarnation,kind,?,grant_id,binding_revision,binding_snapshot,payload_hash,snapshot,state,revision,'2099-01-01T00:00:00Z',updated_at FROM gitlab_operations WHERE id=?`, id, id, op.ID)
		mustNil(t, err)
		_, err = s.db.Exec(`INSERT INTO gitlab_proposal_states VALUES (?,'pending',1,'','')`, id)
		mustNil(t, err)
	}
	all, err := s.GitLabProposals(ctx, "one", "two", "admin")
	mustNil(t, err)
	if len(all) != 100 {
		t.Fatalf("fixture must expose a full inaccessible page to admin: %d", len(all))
	}
	ops, err := s.GitLabProposals(ctx, "one", "", "viewer")
	mustNil(t, err)
	if len(ops) != 1 || ops[0].ID != op.ID {
		t.Fatalf("visible proposal lost behind inaccessible history: count=%d", len(ops))
	}
	ops, err = s.GitLabProposals(ctx, "one", "two", "viewer")
	mustNil(t, err)
	if len(ops) != 0 {
		t.Fatal("swarm filter bypassed visibility")
	}
	_, err = s.db.Exec(`UPDATE users SET disabled=1 WHERE id='viewer'`)
	mustNil(t, err)
	ops, err = s.GitLabProposals(ctx, "one", "", "viewer")
	mustNil(t, err)
	if len(ops) != 0 {
		t.Fatal("disabled viewer received history")
	}
}
