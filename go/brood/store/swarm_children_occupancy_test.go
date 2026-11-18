package store

// BUG: История остановленного прохода удерживает новую инкарнацию исполнителя.
// HYPOTHESIS: Проверка allocation читает все шаги по UID без текущего шага и инкарнации.
// SOLUTION: Используем общий guardActorStep с последним шагом и текущей инкарнацией.
// RESULT: FIXED — история проходов не держит allocation, текущая работа защищена.
// STATUS: FIXED

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestChildAllocationIgnoresStoppedPassHistory(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"inflight", "unconfirmed"} {
		for _, incarnation := range []int64{1, 2} {
			t.Run(fmt.Sprintf("%s/incarnation-%d", state, incarnation), func(t *testing.T) {
				s := dispatchStore(t)
				// Первый проход принадлежит b; привязку меняет только setup до резерва.
				_, err := s.db.Exec(`UPDATE swarm_role_bindings SET actor_uid='b' WHERE role='build'`)
				mustNil(t, err)
				old := attestOne(t, s, "old-worker-step")
				if state == "unconfirmed" {
					mustNil(t, s.UnconfirmActorStep(ctx, old, "receipt lost"))
				} else {
					mustNil(t, s.FinishActorStepDelivery(ctx, old))
				}
				op, err := s.CurrentActorOperation(ctx, "b")
				mustNil(t, err)
				mustNil(t, s.FinishActorOperation(ctx, op, true))
				_, err = s.StopSwarm(ctx, "one", "u", allow)
				mustNil(t, err)
				setActor(t, s, "b", fmt.Sprintf("incarnation=%d", incarnation))
				mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "a"}))
				mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "test", ActorUID: "b"}))
				_, err = startOne(s, "build-first")
				mustNil(t, err)
				parent := attestOne(t, s, "current-parent")
				op, err = s.CurrentActorOperation(ctx, "a")
				mustNil(t, err)
				mustNil(t, s.FinishActorOperation(ctx, op, true))
				v, err := s.GetSwarmStep(ctx, "one", parent.Sequence)
				mustNil(t, err)
				p := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "one", SwarmID: "one", AtespaceUID: "native-one", ActorUID: "a", Incarnation: 1}, Pass: v.Pass, Sequence: v.Sequence, RequestID: v.RequestID}
				c := engineChild(t, s, p, "after-stop")
				started, err := s.StartSwarmChild(ctx, p, c.RequestID, c.Executor, c.Revision, childAllow)
				if err != nil || started.State != "running" || started.Executor.Incarnation != incarnation {
					t.Fatalf("stopped %s history blocked current child: %+v, %v", state, started, err)
				}
			})
		}
	}
}

func TestChildAllocationProtectsLatestGraphWork(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"pending", "attesting", "delivering", "inflight", "unconfirmed"} {
		for _, incarnation := range []int64{1, 2} {
			t.Run(fmt.Sprintf("%s/incarnation-%d", state, incarnation), func(t *testing.T) {
				s := dispatchStore(t)
				if state != "pending" {
					a := reserveOne(t, s, "current-step")
					if state != "attesting" {
						mustNil(t, s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit))
					}
					if state == "inflight" {
						mustNil(t, s.FinishActorStepDelivery(ctx, a))
					}
					if state == "unconfirmed" {
						mustNil(t, s.UnconfirmActorStep(ctx, a, "receipt lost"))
					}
					op, err := s.CurrentActorOperation(ctx, "a")
					mustNil(t, err)
					mustNil(t, s.FinishActorOperation(ctx, op, true))
				}
				setActor(t, s, "a", fmt.Sprintf("incarnation=%d", incarnation))
				err := s.WithTx(ctx, func(ctx context.Context) error { return s.guardChildAllocation(ctx, "a") })
				if incarnation == 1 && !errors.Is(err, ErrCoordinationConflict) {
					t.Fatalf("latest %s work was not protected: %v", state, err)
				}
				if incarnation == 2 && err != nil {
					t.Fatalf("previous incarnation's %s work held current allocation: %v", state, err)
				}
			})
		}
	}
}
