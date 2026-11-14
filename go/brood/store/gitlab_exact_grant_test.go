package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// BUG: Повторная авторизация чтения не умеет закрепить исходный grant.
// HYPOTHESIS: Публичный CheckGitLabAccess всегда заново выбирает grant по ID.
// SOLUTION: CheckGitLabGrantAccess проверяет исходный ID теми же durable правилами.
// RESULT: FIXED — Новый grant не подменяет исходный; expiry/revoke отказывают.
// STATUS: FIXED
func TestGitLabExactGrantAccess(t *testing.T) {
	for _, change := range []string{"additional", "revoked", "expired", "missing", "empty", "principal", "kind", "binding", "human", "inactive"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			original := GitLabGrant{ID: "z-original", Principal: p, BindingRevision: b.Revision, Read: true, ExpiresAt: time.Now().Add(time.Hour)}
			if change == "expired" {
				original.ExpiresAt = time.Now().Add(100 * time.Millisecond)
			}
			original, err := s.CreateGitLabGrant(ctx, "u", original)
			mustNil(t, err)
			selected, err := s.CheckGitLabAccess(ctx, p, "read")
			mustNil(t, err)
			if selected != original {
				t.Fatal("initial read did not select original grant")
			}
			additional := original
			additional.ID = "a-additional"
			additional.ExpiresAt = time.Now().Add(time.Hour)
			_, err = s.CreateGitLabGrant(ctx, "u", additional)
			mustNil(t, err)
			id, kind := original.ID, "read"
			switch change {
			case "revoked":
				mustNil(t, s.RevokeGitLabGrant(ctx, "u", original.ID, original.Revision))
			case "expired":
				<-time.After(time.Until(original.ExpiresAt) + time.Millisecond)
			case "missing":
				id = "missing"
			case "empty":
				id = ""
			case "principal":
				p.Incarnation++
			case "kind":
				kind = "create"
			case "binding":
				_, err = s.PutGitLabBinding(ctx, "admin", b, b.Revision)
				mustNil(t, err)
			case "human":
				mustNil(t, s.SetUserDisabled(ctx, "u", true))
			case "inactive":
				mustNil(t, s.SetSwarmState(ctx, p.SwarmID, "inactive"))
			}
			got, err := s.CheckGitLabGrantAccess(ctx, p, kind, id)
			if change == "additional" {
				mustNil(t, err)
				if got != original {
					t.Fatalf("independent grant replaced original: got=%+v want=%+v", got, original)
				}
			} else if !errors.Is(err, ErrGitLabDenied) || got != (GitLabGrant{}) {
				t.Fatalf("%s exact grant access: got=%+v err=%v", change, got, err)
			}
		})
	}
}
