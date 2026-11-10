package store

// FEATURE: Задача прохода роя (#242), хранилище.
// SPEC: Запуск передаёт задачу прохода: непустой текст до MaxSwarmGoalBytes,
// валидный UTF-8 без управляющих символов, кроме перевода строки и
// табуляции. Задача пишется на первый шаг прохода тем же INSERT, что
// открывает проход, и больше не меняется; каждый шаг прохода называет первый
// шаг своего прохода (Pass) и через него — задачу. Повторный запуск с другой
// задачей начинает новый проход, задачи прошлых проходов остаются при их
// шагах. Недопустимая задача — отказ до записи, на первом и на повторном
// запуске. Резерв шага актора несёт задачу его прохода, а не прошлого.
// IMPLEMENTATION: StartSwarm, beginPass, SwarmSteps, ReserveActorStep;
// колонка swarm_steps.goal и триггер swarm_steps_goal_fence.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// goalStore — startStore, где актор a роли build готов к управлению
// workspace: шаг build можно зарезервировать.
func goalStore(t *testing.T) *Store {
	t.Helper()
	s := startStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	return s
}

func startWithGoal(s *Store, definition, goal string) (Swarm, error) {
	return s.StartSwarm(context.Background(), SwarmStart{SwarmID: "one", UserID: "u", Definition: definition, Commit: startCommit, Goal: goal}, allow)
}

// TestStartSwarmKeepsPassGoal — задача прохода записана на его первом шаге
// как есть, а шаги прохода называют этот шаг; резерв шага актора несёт её.
func TestStartSwarmKeepsPassGoal(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	const goal = "  Fix issue #42: the login page crashes.\n\tKeep the public API.\n"
	_, err := startWithGoal(s, "release", goal)
	mustNil(t, err)
	if v := stepOf(t, s, 1); v.Goal != goal || v.Pass != 1 {
		t.Fatalf("first step of the pass: goal %q pass %d", v.Goal, v.Pass)
	}
	answerNode(t, s, 1, "success")
	if v := stepOf(t, s, 2); v.Goal != "" || v.Pass != 1 || v.NodeID != "build" {
		t.Fatalf("second step of the pass: %+v", v)
	}
	a, err := s.ReserveActorStep(ctx, "one", "req-2", "swarm:one")
	mustNil(t, err)
	if a.Goal != goal || a.Pass != 1 || a.Sequence != 2 {
		t.Fatalf("reserved attempt: goal %q pass %d sequence %d", a.Goal, a.Pass, a.Sequence)
	}
}

// TestRestartCarriesItsOwnGoal — следующий проход идёт со своей задачей:
// шаги прошлого прохода её не получают, задача прошлого прохода не меняется,
// а резерв шага нового прохода несёт новую задачу.
func TestRestartCarriesItsOwnGoal(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	const first, second = "Fix issue #42.", "Fix issue #43 instead."
	_, err := startWithGoal(s, "release", first)
	mustNil(t, err)
	answerNode(t, s, 1, "success")
	stopOne(t, s)
	history := stepRows(t, s, 2)

	_, err = startWithGoal(s, "release", second)
	mustNil(t, err)
	if got := stepRows(t, s, 2); got != history {
		t.Fatalf("restart changed the previous pass:\n%s\nwant\n%s", got, history)
	}
	for sequence, want := range map[int64]struct {
		goal string
		pass int64
	}{1: {first, 1}, 2: {"", 1}, 3: {second, 3}} {
		if v := stepOf(t, s, sequence); v.Goal != want.goal || v.Pass != want.pass {
			t.Fatalf("step %d: goal %q pass %d, want %q %d", sequence, v.Goal, v.Pass, want.goal, want.pass)
		}
	}
	answerNode(t, s, 3, "success")
	a, err := s.ReserveActorStep(ctx, "one", "req-4", "swarm:one")
	mustNil(t, err)
	if a.Goal != second || a.Pass != 3 || a.Sequence != 4 {
		t.Fatalf("attempt of the second pass: goal %q pass %d sequence %d", a.Goal, a.Pass, a.Sequence)
	}
	if v := stepOf(t, s, 4); v.Pass != 3 || v.Goal != "" {
		t.Fatalf("step of the second pass: %+v", v)
	}
}

// TestStartSwarmRefusesInvalidGoal — задача без текста, длиннее предела, не
// UTF-8 или с управляющими символами, кроме перевода строки и табуляции, —
// отказ с причиной до записи, на первом и на повторном запуске.
func TestStartSwarmRefusesInvalidGoal(t *testing.T) {
	for _, tc := range []struct {
		name, goal, reason string
	}{
		{"absent", "", "the goal is empty"},
		{"blank", " \n\t ", "the goal is empty"},
		{"oversized", strings.Repeat("x", MaxSwarmGoalBytes+1), "the goal exceeds 4096 bytes"},
		{"oversized in multibyte text", strings.Repeat("я", MaxSwarmGoalBytes/2) + "!", "the goal exceeds 4096 bytes"},
		{"invalid UTF-8", "fix \xff the parser", "the goal is not valid UTF-8"},
		{"NUL", "fix\x00", "the goal has control characters other than newline and tab"},
		{"carriage return", "fix\r\nit", "the goal has control characters other than newline and tab"},
		{"terminal escape", "\x1b[31mfix", "the goal has control characters other than newline and tab"},
		{"DEL", "fix\x7f", "the goal has control characters other than newline and tab"},
		{"C1 control", "fix\u0085it", "the goal has control characters other than newline and tab"},
	} {
		for _, restart := range []bool{false, true} {
			name := tc.name
			if restart {
				name += " on restart"
			}
			t.Run(name, func(t *testing.T) {
				s := startStore(t)
				if restart {
					mustStart(t, s, "release")
					stopOne(t, s)
				}
				before := dumpTables(t, s, append(executionTables, lifeTables...)...)
				_, err := startWithGoal(s, "release", tc.goal)
				if !errors.Is(err, ErrSwarmGoalInvalid) || err.Error() != "invalid swarm goal: "+tc.reason {
					t.Fatalf("start: %v, want %v: %s", err, ErrSwarmGoalInvalid, tc.reason)
				}
				if after := dumpTables(t, s, append(executionTables, lifeTables...)...); after != before {
					t.Fatalf("refused start wrote:\n%s\nwant\n%s", after, before)
				}
			})
		}
	}
}

// TestStartSwarmAcceptsGoalAtLimit — задача ровно в предел байтов, с
// переводами строк и табуляцией, принимается и хранится без изменений.
func TestStartSwarmAcceptsGoalAtLimit(t *testing.T) {
	s := startStore(t)
	goal := strings.Repeat("я", MaxSwarmGoalBytes/2-2) + "\n\t.."
	if len(goal) != MaxSwarmGoalBytes {
		t.Fatalf("fixture goal is %d bytes", len(goal))
	}
	_, err := startWithGoal(s, "release", goal)
	mustNil(t, err)
	if v := stepOf(t, s, 1); v.Goal != goal {
		t.Fatalf("stored goal differs: %q", v.Goal)
	}
	if err := ValidSwarmGoal(goal); err != nil {
		t.Fatalf("goal at the limit: %v", err)
	}
}

// TestSwarmStepSchemaHoldsGoal — схема держит предел задачи и её
// неизменность мимо кода: задачу нельзя переписать, стереть или дописать
// шагу после вставки.
func TestSwarmStepSchemaHoldsGoal(t *testing.T) {
	insert := func(goal string) string {
		return `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,goal) VALUES ('one',9,'ask','stopped','` + goal + `')`
	}
	for _, tc := range []struct {
		name, statement string
		accepted        bool
	}{
		{"rewritten", `UPDATE swarm_steps SET goal='forged' WHERE swarm_id='one' AND sequence=1`, false},
		{"erased", `UPDATE swarm_steps SET goal=NULL WHERE swarm_id='one' AND sequence=1`, false},
		{"added to a later step", `UPDATE swarm_steps SET goal='late' WHERE swarm_id='one' AND sequence=2`, false},
		{"empty", insert(""), false},
		{"oversized", insert(strings.Repeat("x", MaxSwarmGoalBytes+1)), false},
		// Контроль: те же строки без нарушения проходят, отказы выше — из-за задачи.
		{"other column of the first step", `UPDATE swarm_steps SET finished_at='later' WHERE swarm_id='one' AND sequence=1`, true},
		{"step with a goal at the limit", insert(strings.Repeat("x", MaxSwarmGoalBytes)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := goalStore(t)
			mustStart(t, s, "release")
			answerNode(t, s, 1, "success")
			if _, err := s.db.Exec(tc.statement); (err == nil) != tc.accepted {
				t.Fatalf("%s: %v, accepted %v", tc.name, err, tc.accepted)
			}
			if v := stepOf(t, s, 1); v.Goal != startGoal {
				t.Fatalf("goal changed: %q", v.Goal)
			}
		})
	}
}
