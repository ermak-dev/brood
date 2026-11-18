package store

import (
	"context"
	"errors"
	"testing"
)

func TestDurableTransactionCommitsWithFullWALSyncAndRestoresConnection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var before int
	mustNil(t, s.db.QueryRow(`PRAGMA synchronous`).Scan(&before))
	mustNil(t, s.WithDurableTx(ctx, func(ctx context.Context) error {
		var mode int
		if err := s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode); err != nil {
			return err
		}
		if mode != 2 {
			t.Fatalf("external effect reservation uses synchronous=%d, want FULL", mode)
		}
		return s.CreateUser(ctx, User{ID: "durable", Username: "durable", PasswordHash: "h"})
	}))
	if _, err := s.GetUser(ctx, "durable"); err != nil {
		t.Fatal("durable commit was not visible")
	}
	var after int
	mustNil(t, s.db.QueryRow(`PRAGMA synchronous`).Scan(&after))
	if after != before {
		t.Fatal("effect transaction changed ordinary transaction settings")
	}
}

func TestDurableTransactionRollbackAndPanicDoNotLeakConnection(t *testing.T) {
	for _, failure := range []string{"error", "panic", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			s := newTestStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("callback refused")
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				err := s.WithDurableTx(ctx, func(ctx context.Context) error {
					if err := s.CreateUser(ctx, User{ID: "rolled-back", Username: "rolled-back", PasswordHash: "h"}); err != nil {
						return err
					}
					switch failure {
					case "panic":
						panic("callback panic")
					case "cancel":
						cancel()
						return ctx.Err()
					}
					return want
				})
				if failure == "error" && !errors.Is(err, want) {
					t.Errorf("callback error lost: %v", err)
				}
				if failure == "cancel" && !errors.Is(err, context.Canceled) {
					t.Errorf("cancellation lost: %v", err)
				}
			}()
			if panicked != (failure == "panic") {
				t.Fatal("panic propagation changed")
			}
			if _, err := s.GetUser(context.Background(), "rolled-back"); !errors.Is(err, ErrNotFound) {
				t.Fatal("failed effect transaction committed")
			}
			var mode int
			mustNil(t, s.db.QueryRow(`PRAGMA synchronous`).Scan(&mode))
			if mode != 1 {
				t.Fatal("failed effect transaction leaked FULL into ordinary connection")
			}
			mustNil(t, s.CreateUser(context.Background(), User{ID: "next", Username: "next", PasswordHash: "h"}))
		})
	}
}

func TestDurableTransactionCommitSurvivesStoreReopen(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	var seq int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
	mustNil(t, s.WithDurableTx(ctx, func(ctx context.Context) error {
		return s.CreateUser(ctx, User{ID: "reopened", Username: "reopened", PasswordHash: "h"})
	}))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	if _, err := reopened.GetUser(ctx, "reopened"); err != nil {
		t.Fatal("durable reservation did not survive reopen")
	}
}

func TestDurableTransactionCannotReturnPermissionInsideOuterTransaction(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		called := false
		err := s.WithDurableTx(ctx, func(context.Context) error { called = true; return nil })
		if !errors.Is(err, ErrDurableTransactionNested) || called {
			t.Fatal("effect permission escaped before outer transaction commit")
		}
		return nil
	}))
}
