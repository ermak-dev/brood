package store

// BUG: Добавление grant меняет authority при резервировании pinned намерения.
// HYPOTHESIS: Резерв ищет первый grant вместо выбранного вызывающим кодом.
// SOLUTION: Exact API передаёт grant ID в общую транзакционную проверку.
// RESULT: FIXED — Добавление authority не меняет grant; отзыв запрещает резерв.
// STATUS: FIXED

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func exactReservationGrants(t *testing.T, s *Store, p GitLabPrincipal, b GitLabBinding) {
	t.Helper()
	for _, id := range []string{"z", "a"} {
		_, err := s.CreateGitLabGrant(context.Background(), "u", GitLabGrant{ID: id, Principal: p, BindingRevision: b.Revision, Create: true, ExpiresAt: time.Now().Add(time.Hour)})
		mustNil(t, err)
	}
}
func TestGitLabReservationKeepsSelectedGrant(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, b := gitlabFixture(t, s)
	exactReservationGrants(t, s, p, b)
	op, err := s.ReserveGitLabMutationWithGrant(ctx, p, "create", "pinned", "z", []byte("payload"))
	mustNil(t, err)
	if op.GrantID != "z" || op.Binding != b || op.Principal != p {
		t.Fatalf("pinned authority substituted: %+v", op)
	}
	retry, err := s.ReserveGitLabMutationWithGrant(ctx, p, "create", "pinned", "z", []byte("payload"))
	mustNil(t, err)
	if retry != op {
		t.Fatal("retry changed immutable intent")
	}
	for _, grant := range []string{"", "missing", "a"} {
		gitlabDeniedUnchanged(t, s, func() error {
			_, err := s.ReserveGitLabMutationWithGrant(ctx, p, "create", "pinned", grant, []byte("payload"))
			return err
		})
	}
	gitlabDeniedUnchanged(t, s, func() error {
		_, err := s.ReserveGitLabMutationWithGrant(ctx, p, "create", "pinned", "z", []byte("changed"))
		return err
	})
	g, err := s.GetGitLabGrant(ctx, "z")
	mustNil(t, err)
	mustNil(t, s.RevokeGitLabGrant(ctx, "u", "z", g.Revision))
	for _, request := range []string{"pinned", "new"} {
		gitlabDeniedUnchanged(t, s, func() error {
			_, err := s.ReserveGitLabMutationWithGrant(ctx, p, "create", request, "z", []byte("payload"))
			if !errors.Is(err, ErrGitLabDenied) {
				t.Fatalf("revoked pinned authority: %v", err)
			}
			return err
		})
	}
	gitlabDeniedUnchanged(t, s, func() error { _, err := s.TransitionGitLabOperation(ctx, op.ID, op.Revision, "sending"); return err })
	if _, err := s.CheckGitLabGrantAccess(ctx, p, "create", "a"); err != nil {
		t.Fatal("replacement must remain valid", err)
	}
}

func TestGitLabExactReservationConcurrentCAS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.sqlite")
	s, err := Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	p, b := gitlabFixture(t, s)
	exactReservationGrants(t, s, p, b)
	other, err := Open(ctx, path)
	mustNil(t, err)
	defer other.Close()
	var wg sync.WaitGroup
	ops := make(chan GitLabOperation, 12)
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			db := []*Store{s, other}[i%2]
			op, err := db.ReserveGitLabMutationWithGrant(ctx, p, "create", "concurrent", "z", []byte("payload"))
			ops <- op
			errs <- err
		})
	}
	wg.Wait()
	close(ops)
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	var original GitLabOperation
	for op := range ops {
		if op.GrantID != "z" || op.Binding != b {
			t.Fatalf("authority drift: %+v", op)
		}
		if original.ID != "" && original != op {
			t.Fatal("duplicate reservation")
		}
		original = op
	}
	claims := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			_, err := []*Store{s, other}[i%2].TransitionGitLabOperation(ctx, original.ID, original.Revision, "sending")
			claims <- err
		})
	}
	wg.Wait()
	close(claims)
	wins := 0
	for err := range claims {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrGitLabConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("sending winners = %d", wins)
	}
}
