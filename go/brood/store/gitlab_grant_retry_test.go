package store

// BUG: Повтор выдачи grant оставляет неизвестное разрешение после отзыва ответа.
// HYPOTHESIS: CreateGitLabGrant заменяет переданный стабильный ID случайным.
// SOLUTION: Сохраняем ID и сравниваем неизменяемые поля под транзакцией, без новой схемы.
// RESULT: FIXED — Повтор и рестарт сохраняют единственный grant; отзыв окончателен.
// STATUS: FIXED

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGitLabGrantStableIdentityRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retry.sqlite")
	s, err := Open(ctx, path)
	mustNil(t, err)
	t.Cleanup(func() { _ = s.Close() })
	p, b := gitlabFixture(t, s)
	input := GitLabGrant{ID: "grant-request-1", Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	first, err := s.CreateGitLabGrant(ctx, "u", input)
	mustNil(t, err)
	retry, err := s.CreateGitLabGrant(ctx, "u", input)
	mustNil(t, err)
	if first.ID != input.ID || retry != first {
		t.Fatalf("exact retry changed grant identity: requested=%q first=%+v retry=%+v", input.ID, first, retry)
	}
	if rows := tableRows(t, s.db, "gitlab_grants"); len(rows) != 1 {
		t.Fatalf("exact retry created %d authority rows, want 1", len(rows))
	}
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	retry, err = s.CreateGitLabGrant(ctx, "u", input)
	mustNil(t, err)
	if retry != first {
		t.Fatalf("restart changed grant: %+v", retry)
	}
	mustNil(t, s.RevokeGitLabGrant(ctx, "u", retry.ID, retry.Revision))
	gitlabDeniedUnchanged(t, s, func() error {
		_, err := s.CreateGitLabGrant(ctx, "u", input)
		if !errors.Is(err, ErrGitLabConflict) {
			t.Errorf("retry after revoke: %v, want conflict", err)
		}
		return err
	})
	if _, err := s.CheckGitLabAccess(ctx, p, "read"); !errors.Is(err, ErrGitLabDenied) {
		t.Fatalf("revoking returned grant left read authority: %v", err)
	}
}

func TestGitLabGrantRetryPayloadConflicts(t *testing.T) {
	for _, change := range []string{"project", "swarm", "atespace", "actor", "incarnation", "human", "binding", "read", "create", "propose", "expiry"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			input := GitLabGrant{ID: "stable-grant", Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour)}
			_, err := s.CreateGitLabGrant(ctx, "u", input)
			mustNil(t, err)
			human := "u"
			switch change {
			case "project":
				input.Principal.ProjectID = "other"
			case "swarm":
				input.Principal.SwarmID = "other"
			case "atespace":
				input.Principal.AtespaceUID = "other"
			case "actor":
				input.Principal.ActorUID = "other"
			case "incarnation":
				input.Principal.Incarnation++
			case "human":
				human = "admin"
			case "binding":
				input.BindingRevision++
			case "read":
				input.Read = false
				input.Create = true
			case "create":
				input.Create = true
			case "propose":
				input.Propose = true
			case "expiry":
				input.ExpiresAt = input.ExpiresAt.Add(time.Second)
			}
			gitlabDeniedUnchanged(t, s, func() error {
				_, err := s.CreateGitLabGrant(ctx, human, input)
				if !errors.Is(err, ErrGitLabConflict) {
					t.Errorf("changed %s: %v, want conflict", change, err)
				}
				return err
			})
		})
	}
}

func TestGitLabGrantConcurrentStableIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.sqlite")
	s, err := Open(ctx, path)
	mustNil(t, err)
	t.Cleanup(func() { _ = s.Close() })
	p, b := gitlabFixture(t, s)
	other, err := Open(ctx, path)
	mustNil(t, err)
	t.Cleanup(func() { _ = other.Close() })
	stores := []*Store{s, other}
	input := GitLabGrant{ID: "concurrent-grant", Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 12 {
		s := stores[i%len(stores)]
		wg.Go(func() {
			<-start
			g, err := s.CreateGitLabGrant(ctx, "u", input)
			if err != nil || g.ID != input.ID || g.Revision != 1 {
				t.Errorf("duplicate creation: %+v %v", g, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if rows := tableRows(t, s.db, "gitlab_grants"); len(rows) != 1 {
		t.Fatalf("concurrent retry created %d authority rows, want 1", len(rows))
	}
	mustNil(t, s.RevokeGitLabGrant(ctx, "u", input.ID, 1))
	for i := range 12 {
		s := stores[i%len(stores)]
		wg.Go(func() {
			_, err := s.CreateGitLabGrant(ctx, "u", input)
			if !errors.Is(err, ErrGitLabConflict) {
				t.Errorf("concurrent revoked retry: %v", err)
			}
		})
	}
	wg.Wait()
	g, err := s.GetGitLabGrant(ctx, input.ID)
	mustNil(t, err)
	if !g.Revoked || g.Revision != 2 || !g.ExpiresAt.Equal(input.ExpiresAt) {
		t.Fatalf("revoked retry changed grant: %+v", g)
	}
	if rows := tableRows(t, s.db, "gitlab_grants"); len(rows) != 1 {
		t.Fatalf("revoked retry created %d rows", len(rows))
	}
}

func TestGitLabGrantRetryCurrentAuthority(t *testing.T) {
	for _, change := range []string{"disabled", "owner", "binding", "actor", "swarm", "repository", "expiry"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			input := GitLabGrant{ID: "current-grant", Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour)}
			_, err := s.CreateGitLabGrant(ctx, "u", input)
			mustNil(t, err)
			switch change {
			case "disabled":
				mustNil(t, s.SetUserDisabled(ctx, "u", true))
			case "owner":
				_, err = s.db.Exec(`DELETE FROM swarm_members WHERE swarm_id='one' AND user_id='u'`)
				mustNil(t, err)
			case "binding":
				_, err = s.PutGitLabBinding(ctx, "admin", b, b.Revision)
				mustNil(t, err)
			case "actor":
				_, err = s.db.Exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, p.ActorUID)
				mustNil(t, err)
			case "swarm":
				mustNil(t, s.SetSwarmState(ctx, p.SwarmID, "inactive"))
			case "repository":
				mustNil(t, s.SetProjectRepository(ctx, p.ProjectID, ""))
			case "expiry":
				// Срок не редактируем: ждём истечения уже сохранённого короткого grant.
				input.ID = "short-grant"
				input.ExpiresAt = time.Now().Add(50 * time.Millisecond)
				_, err = s.CreateGitLabGrant(ctx, "u", input)
				mustNil(t, err)
				time.Sleep(time.Until(input.ExpiresAt) + time.Millisecond)
			}
			gitlabDeniedUnchanged(t, s, func() error { _, err := s.CreateGitLabGrant(ctx, "u", input); return err })
		})
	}
}

func TestGitLabGrantStableIDValidation(t *testing.T) {
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	for _, id := range []string{"with spaces", "with/slash", fmt.Sprintf("%0129d", 0)} {
		gitlabDeniedUnchanged(t, s, func() error {
			_, err := s.CreateGitLabGrant(context.Background(), "u", GitLabGrant{ID: id, Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour)})
			return err
		})
	}
}
