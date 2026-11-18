package store

// BUG: Завершение процесса между reserve и полной регистрацией оставляет
// долговечный неполный join без исполняющихся детей.
// HYPOTHESIS: Recovery должно работать и с committed WAL после выхода без Close.
// SOLUTION: Явный human abort читает сохранённую группу и атомарно снимает
// только неисполненные обязательства, не полагаясь на память диспетчера.
// RESULT: FIXED — unclean exit сохраняет partial state; human recovery
// снимает missing-slot hold без отправки/повтора инструкций и изменения policy.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestSwarmChildAbortCrashRecovery(t *testing.T) {
	ctx := context.Background()
	type crashInput struct {
		Path, State string
		Parent      SwarmChildParent
		Executor    SwarmChildPrincipal
	}
	if encoded := os.Getenv("BROOD_TEST_CHILD_ABORT_CRASH"); encoded != "" {
		var in crashInput
		mustNil(t, json.Unmarshal([]byte(encoded), &in))
		s, err := Open(ctx, in.Path)
		mustNil(t, err)
		c, err := s.ReserveSwarmChild(ctx, in.Parent, "crashed", "Committed unstarted child", in.Executor, childAllow)
		mustNil(t, err)
		if in.State == "cancelled" {
			_, err = s.CancelSwarmChild(ctx, in.Parent, c.RequestID, c.Revision, childAllow)
			mustNil(t, err)
		}
		// Не запускаем ни Close, ни cleanup: следующий процесс читает committed WAL.
		os.Exit(73)
	}
	for _, state := range []string{"pending", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 2, 0)
			session := childRecoverySession(t, s, "u")
			var path, dbname string
			var seq int
			mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbname, &path))
			encoded, err := json.Marshal(crashInput{Path: path, State: state, Parent: p, Executor: e})
			mustNil(t, err)
			command := exec.Command(os.Args[0], "-test.run=^TestSwarmChildAbortCrashRecovery$", "-test.count=1")
			command.Env = append(os.Environ(), "BROOD_TEST_CHILD_ABORT_CRASH="+string(encoded))
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("unclean writer exit: %v\n%s", err, output)
			}
			s = reopenChildStore(t, s)
			join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if len(join.Children) != 1 || join.Children[0].State != state || join.State != "open" {
				t.Fatalf("unclean exit lost committed partial state: %+v", join)
			}
			if err := s.CheckActorChildObligations(ctx, p.Principal.ActorUID); !errors.Is(err, ErrCoordinationConflict) {
				t.Fatalf("restart bypassed missing-slot automatic hold: %v", err)
			}
			abort, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
			mustNil(t, err)
			if abort.State != "aborted" || abort.Evaluation.Open != 0 || abort.Policy != join.Policy || len(abort.Children) != 1 || abort.Children[0].ID != join.Children[0].ID || abort.Children[0].State != "cancelled" {
				t.Fatalf("recovery after unclean exit: %+v", abort)
			}
			_, err = s.StopSwarm(ctx, "one", "u", allow)
			mustNil(t, err)
			t.Logf("unclean writer exit with %s child: automatic hold retained before recovery; explicit abort releases missing slot", state)
		})
	}
}
