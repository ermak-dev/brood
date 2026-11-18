package store

// BUG: Неполный join навсегда держит родителя после отзыва права создать детей.
// HYPOTHESIS: Неизменяемый total учитывает отсутствующий слот, но у группы нет
// явной отмены до первого исполнения с независимыми текущими правами человека.
// SOLUTION: Транзакционный owner/admin abort сохраняет total и историю,
// отменяет лишь pending и снимает отсутствующие слоты отдельным terminal state.
// RESULT: FIXED — missing-slot barrier снимается только явным human abort;
// до recovery завершение и автоматическая мутация отказывают; owner stop сохраняет hold.
// STATUS: FIXED

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Интерфейс оставляет регрессию исполнимой и на версии без recovery API.
func abortChildJoin(t *testing.T, s *Store, p SwarmChildParent, session string, expected int64, authorize SwarmChildAuthorize) (SwarmChildJoin, error) {
	t.Helper()
	recovery, ok := any(s).(interface {
		AbortSwarmChildJoin(context.Context, SwarmChildParent, string, int64, SwarmChildAuthorize) (SwarmChildJoin, error)
	})
	if !ok {
		t.Fatal("incomplete unstarted join has no authorized abort; absent slot holds parent forever")
	}
	return recovery.AbortSwarmChildJoin(context.Background(), p, session, expected, authorize)
}

func TestSwarmChildIncompleteJoinAuthorizedAbort(t *testing.T) {
	ctx := context.Background()
	for _, close := range []string{"complete", "stop"} {
		t.Run(close, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 2, 0)
			c := reserveChild(t, s, p, e, "only")
			c, err := s.CancelSwarmChild(ctx, p, c.RequestID, c.Revision, childAllow)
			mustNil(t, err)
			deny := func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			_, err = s.ReserveSwarmChild(ctx, p, "second", "task", e, deny)
			if !errors.Is(err, ErrSwarmChildDenied) {
				t.Fatalf("new scheduling: %v", err)
			}
			join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if join.Evaluation.Open != 1 {
				t.Fatalf("missing slot not reproduced: %+v", join)
			}
			if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", ""); err == nil {
				t.Fatal("parent barrier missing before recovery")
			}
			if err := s.CheckActorChildObligations(ctx, p.Principal.ActorUID); !errors.Is(err, ErrCoordinationConflict) {
				t.Fatalf("automatic hold missing before recovery: %v", err)
			}
			mustNil(t, s.CreateSession(ctx, Session{TokenHash: "recovery", UserID: "u"}))
			recovered, err := abortChildJoin(t, s, p, "recovery", 1, childAllow)
			mustNil(t, err)
			if recovered.Evaluation.State != "aborted" || recovered.Evaluation.Open != 0 || recovered.Policy != join.Policy || !reflect.DeepEqual(recovered.Children, []SwarmChildTask{c}) {
				t.Fatalf("abort lost history or did not release absent slot: %+v", recovered)
			}
			if close == "complete" {
				mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", ""))
			} else {
				_, err = s.StopSwarm(ctx, "one", "u", allow)
				mustNil(t, err)
			}
			retry, err := abortChildJoin(t, s, p, "recovery", 1, childAllow)
			mustNil(t, err)
			if !reflect.DeepEqual(recovered, retry) {
				t.Fatal("exact retry changed abort history")
			}
			t.Log("absent obligations 1 -> 0; parent closure refused -> accepted; policy and cancelled child unchanged")
		})
	}
}
