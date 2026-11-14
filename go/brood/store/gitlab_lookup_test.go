package store

import (
	"context"
	"errors"
	"testing"
)

// FEATURE: Поиск исторического GitLab intent по точному principal/request.
// SPEC: Чужая область скрыта; чтение не создаёт intent и не означает authority.
// IMPLEMENTATION: Поиск по всем полям principal без изменения схемы.
// RESULT: IMPLEMENTED — точный lookup и отсутствие authority после отзыва проверены.
// STATUS: DONE
func TestFindGitLabOperation(t *testing.T) {
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	g := gitlabGrant(t, s, p, b)
	ctx := context.Background()
	if _, err := s.FindGitLabOperation(ctx, p, "create", "key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	original, err := gitlabReserve(s, p, "key")
	mustNil(t, err)
	found, err := s.FindGitLabOperation(ctx, p, "create", "key")
	mustNil(t, err)
	if found != original {
		t.Fatalf("wrong operation: %+v", found)
	}
	for _, field := range []string{"project", "swarm", "atespace", "actor", "incarnation", "kind", "request"} {
		foreign := p
		kind, request := "create", "key"
		switch field {
		case "project":
			foreign.ProjectID = "other"
		case "swarm":
			foreign.SwarmID = "other"
		case "atespace":
			foreign.AtespaceUID = "other"
		case "actor":
			foreign.ActorUID = "other"
		case "incarnation":
			foreign.Incarnation++
		case "kind":
			kind = "propose"
		case "request":
			request = "other"
		}
		if _, err := s.FindGitLabOperation(ctx, foreign, kind, request); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s exposed intent: %v", field, err)
		}
	}
	mustNil(t, s.RevokeGitLabGrant(ctx, "u", g.ID, g.Revision))
	found, err = s.FindGitLabOperation(ctx, p, "create", "key")
	mustNil(t, err)
	if found != original {
		t.Fatal("revocation erased history")
	}
	if _, err = s.CheckGitLabGrantAccess(ctx, p, "create", found.GrantID); !errors.Is(err, ErrGitLabDenied) {
		t.Fatalf("history authorized: %v", err)
	}
}
