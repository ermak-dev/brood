package store

// FEATURE: Commit источника на проход (#229), хранилище.
// SPEC: Граф, его личность и origin закреплены на рой первым запуском; commit
// источника закрепляется на проход: каждый запуск называет commit, и первый
// шаг прохода хранит его рядом с задачей прохода, неизменно. Повторный запуск
// с другим commit того же определения открывает проход с этим commit, шаги
// прошлых проходов и их commit не меняются. Другое определение и repository
// проекта, отличный от origin роя, — отказ до записи. Подтверждение checkout,
// доставка и исход шага сверяются с commit его прохода, а не роя.
// IMPLEMENTATION: StartSwarm (pinnedPass, startSource, beginPass), stepPass,
// ReserveActorStep, RecordActorStepAttestation, CompletePipelineActorVisit;
// колонка swarm_steps.source_commit, триггеры swarm_steps_pass_fence и
// swarm_steps_attested(_insert).
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// nextCommit — commit интеграционной ветки ко второму проходу.
var nextCommit = strings.Repeat("b", 40)

func startAt(s *Store, definition, commit string) (Swarm, error) {
	return s.StartSwarm(context.Background(), SwarmStart{SwarmID: "one", UserID: "u", Definition: definition, Commit: commit, Goal: startGoal}, allow)
}

// TestRestartPinsCommitToPass — повторный запуск с новым commit открывает
// проход на нём: граф и его личность прежние, шаги прошлого прохода и их
// commit не меняются, рой показывает commit текущего прохода.
func TestRestartPinsCommitToPass(t *testing.T) {
	s := dispatchStore(t)
	first, err := s.GetSwarm(context.Background(), "one")
	mustNil(t, err)
	stopOne(t, s)
	history := stepRows(t, s, 1)

	again, err := startAt(s, "build-first", nextCommit)
	mustNil(t, err)
	if string(again.Graph) != string(first.Graph) || again.DefinitionVersion != first.DefinitionVersion || again.DefinitionRevision != first.DefinitionRevision {
		t.Fatalf("restart replaced the graph snapshot: %+v, was %+v", again, first)
	}
	if again.Source == nil || *again.Source != (PipelineSource{Origin: startOrigin, Commit: nextCommit}) {
		t.Fatalf("swarm source after restart: %+v", again.Source)
	}
	if got := stepRows(t, s, 1); got != history {
		t.Fatalf("restart changed the previous pass:\n%s\nwant\n%s", got, history)
	}
	if v := stepOf(t, s, 1); v.SourceCommit != startCommit || v.Pass != 1 {
		t.Fatalf("first pass: %+v", v)
	}
	if v := stepOf(t, s, 2); v.SourceCommit != nextCommit || v.Pass != 2 {
		t.Fatalf("second pass: %+v", v)
	}

	// Тот же commit ещё раз — повтор прохода, не отказ.
	stopOne(t, s)
	_, err = startAt(s, "build-first", nextCommit)
	mustNil(t, err)
	if v := stepOf(t, s, 3); v.SourceCommit != nextCommit || v.Pass != 3 {
		t.Fatalf("repeated pass: %+v", v)
	}
}

// TestRestartRefusesForeignSnapshot — повторный запуск не меняет то, что
// закреплено на рой: другое определение, repository проекта, отличный от
// origin роя, и запуск без commit — отказ с причиной и без записи.
func TestRestartRefusesForeignSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*testing.T, *Store)
		definition string
		commit     string
		want       error
		text       string
	}{
		{name: "another definition", definition: "release", commit: nextCommit, want: ErrSwarmGraphPinned, text: `"build-first"`},
		{name: "another repository", setup: func(t *testing.T, s *Store) {
			mustNil(t, s.SetProjectRepository(context.Background(), "one", "https://git.example.org/team/other.git"))
		}, definition: "build-first", commit: nextCommit, want: ErrSwarmGraphPinned, text: startOrigin},
		{name: "no commit", definition: "build-first", want: ErrSwarmSourceInvalid, text: "commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dispatchStore(t)
			stopOne(t, s)
			if tc.setup != nil {
				tc.setup(t, s)
			}
			before := dumpTables(t, s, append(executionTables, lifeTables...)...)
			_, err := startAt(s, tc.definition, tc.commit)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("restart: %v, want %v naming %q", err, tc.want, tc.text)
			}
			if after := dumpTables(t, s, append(executionTables, lifeTables...)...); after != before {
				t.Fatalf("refused restart wrote:\n%s\nwant\n%s", after, before)
			}
		})
	}
}

// TestAttestationUsesPassCommit — шаг нового прохода уходит актору только с
// подтверждённым commit своего прохода: commit прошлого прохода — отказ, свой
// — доставка и исход.
func TestAttestationUsesPassCommit(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	stopOne(t, s)
	_, err := startAt(s, "build-first", nextCommit)
	mustNil(t, err)
	a := reserveOne(t, s, "req-2")
	if a.Sequence != 2 || a.Source != (PipelineSource{Origin: startOrigin, Commit: nextCommit}) {
		t.Fatalf("attempt of the second pass: sequence %d source %+v", a.Sequence, a.Source)
	}
	if err := s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("attestation of the previous pass commit: %v", err)
	}
	mustNil(t, s.RecordActorStepAttestation(ctx, a, startOrigin, nextCommit))
	mustNil(t, s.FinishActorStepDelivery(ctx, a))
	mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 2, "req-2", "a", 1, "success", ""))
	if v := stepOf(t, s, 2); v.State != "completed" || v.AttestedCommit != nextCommit {
		t.Fatalf("step of the second pass: %+v", v)
	}
}

// TestSwarmStepSchemaHoldsPassCommit — схема держит commit прохода мимо кода:
// он есть ровно у первого шага прохода, вместе с задачей, не переписывается,
// не стирается и не дописывается; подтверждение шага сверяется с commit его
// прохода, а не последнего.
func TestSwarmStepSchemaHoldsPassCommit(t *testing.T) {
	insert := func(goal, commit string) string {
		return `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,goal,source_commit) VALUES ('one',9,'build','stopped',` + goal + `,` + commit + `)`
	}
	old, next := "'"+startCommit+"'", "'"+nextCommit+"'"
	for _, tc := range []struct {
		name, statement string
		accepted        bool
	}{
		{"rewritten", `UPDATE swarm_steps SET source_commit=` + old + ` WHERE swarm_id='one' AND sequence=2`, false},
		{"erased", `UPDATE swarm_steps SET source_commit=NULL WHERE swarm_id='one' AND sequence=2`, false},
		{"added to a later step", `UPDATE swarm_steps SET source_commit=` + next + ` WHERE swarm_id='one' AND sequence=3`, false},
		{"commit without goal", insert("NULL", next), false},
		{"goal without commit", insert("'goal'", "NULL"), false},
		{"malformed commit", insert("'goal'", "'HEAD'"), false},
		{"previous pass attested with the new commit", `UPDATE swarm_steps SET state='inflight',attested_commit=` + next + ` WHERE swarm_id='one' AND sequence=1`, false},
		{"new pass attested with the previous commit", `UPDATE swarm_steps SET state='delivering',attested_commit=` + old + ` WHERE swarm_id='one' AND sequence=3`, false},
		{"new pass inserted with the previous commit", `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,attested_commit) VALUES ('one',9,'build','inflight',` + old + `)`, false},
		// Контроль: те же строки без нарушения проходят.
		{"pass opened with goal and commit", insert("'goal'", next), true},
		{"previous pass attested with its commit", `UPDATE swarm_steps SET state='inflight',attested_commit=` + old + ` WHERE swarm_id='one' AND sequence=1`, true},
		{"new pass attested with its commit", `UPDATE swarm_steps SET state='delivering',attested_commit=` + next + ` WHERE swarm_id='one' AND sequence=3`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проход 1 на startCommit, проход 2 на nextCommit: шаг 2 — его
			// первый шаг, шаг 3 — следующий шаг того же прохода.
			s := goalStore(t)
			_, err := startAt(s, "release", startCommit)
			mustNil(t, err)
			stopOne(t, s)
			_, err = startAt(s, "release", nextCommit)
			mustNil(t, err)
			answerNode(t, s, 2, "success")
			if _, err := s.db.Exec(tc.statement); (err == nil) != tc.accepted {
				t.Fatalf("%s: %v, accepted %v", tc.name, err, tc.accepted)
			}
			if v := stepOf(t, s, 2); v.SourceCommit != nextCommit {
				t.Fatalf("pass commit changed: %q", v.SourceCommit)
			}
		})
	}
}

// TestReserveRefusesStepWithoutPassCommit — шаг роя с origin, но вне прохода,
// открытого запуском, подтвердить нечем: резерв закрывает его как refused с
// причиной до привязки роли и workspace не занимает.
func TestReserveRefusesStepWithoutPassCommit(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	mustNil(t, s.AttachSwarmGraph(ctx, "s", []byte(actorStartGraph), "release", 1, "rev", startOrigin))
	mustNil(t, s.SetSwarmExecution(ctx, "s", "running"))
	mustNil(t, s.SetSwarmState(ctx, "s", "active"))
	_, err := s.db.Exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('s',1,'actor','pending')`)
	mustNil(t, err)
	if _, err := s.ReserveActorStep(ctx, "s", "req-1", "swarm:s"); !errors.Is(err, ErrActorStepRefused) || !strings.Contains(err.Error(), "no source commit") {
		t.Fatalf("reserve without a pass commit: %v", err)
	}
	steps, err := s.SwarmSteps(ctx, "s")
	mustNil(t, err)
	if len(steps) != 1 || steps[0].State != "refused" || !strings.Contains(steps[0].Detail, "no source commit") {
		t.Fatalf("step without a pass commit: %+v", steps)
	}
	var leases int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM coordination_claims`).Scan(&leases))
	if leases != 0 {
		t.Fatalf("refused reservation took %d workspace leases", leases)
	}
}
