package store

// FEATURE: Нативное создание актора под узел закрыто (#208).
// SPEC: Узел называет роль, рой связывает её с существующим актором и не
// создаёт актора на узел (#204). Протокол нативного создания —
// ReservePipelineNativeActor (разрешение создать актора под посещение),
// BindPipelineNativeActor (запись UID созданного актора в шаг) и
// MarkPipelineNativeActorUncertain (неопределённый исход создания) —
// отказывает ErrPipelineNodeActorCreation до любого чтения и записи.
// Неопределённость любого шага inflight по-прежнему фиксирует
// RecoverPipelineActions.
// IMPLEMENTATION: Функции-заглушки с явной ошибкой.
// RESULT: IMPLEMENTED — шаг не получает ни нового нативного актора, ни
// UID в обход привязки роли, даже привязанного актора и даже для строки
// нативного резерва, записанной прежней схемой.
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// nativeStep — шаг 1 роя «s»: актор, UID, инкарнация, состояние и нативное имя.
func nativeStep(t *testing.T, s *Store) (int, string) {
	t.Helper()
	var steps int
	var row string
	if err := s.db.QueryRow(`SELECT count(*),COALESCE(MAX(COALESCE(actor_uid,'-')||'@'||COALESCE(actor_incarnation,0)||' '||state||' '||COALESCE(native_actor_name,'-')),'') FROM swarm_steps WHERE swarm_id='s'`).Scan(&steps, &row); err != nil {
		t.Fatal(err)
	}
	return steps, row
}

func TestPipelineNativeActorCreationClosed(t *testing.T) {
	ctx := context.Background()
	reservation := PipelineNativeReservation{SwarmID: "s", RunID: "s", Sequence: 1, NodeID: "actor", RequestID: "req", TemplateName: "development", TemplateUID: "template-uid", TemplateDigest: "sha256:" + strings.Repeat("a", 64)}
	// setup — граф запущен, роль стартового узла привязана к живому
	// актору: всё, что прежде допускало резервирование.
	setup := func(t *testing.T) (*Store, ManagedActor) {
		t.Helper()
		s := sourceTestStore(t)
		if err := attachTestGraph(ctx, s, "s", actorStartGraph); err != nil {
			t.Fatal(err)
		}
		return s, roleActor(t, s, "s", "actor-a", "build")
	}
	t.Run("reservation", func(t *testing.T) {
		s, _ := setup(t)
		got, err := s.ReservePipelineNativeActor(ctx, reservation)
		if !errors.Is(err, ErrPipelineNodeActorCreation) || got.ActorName != "" || got.Replay {
			t.Fatalf("native reservation: %+v %v, want %v", got, err, ErrPipelineNodeActorCreation)
		}
		if steps, _ := nativeStep(t, s); steps != 0 {
			t.Fatalf("closed reservation left %d steps", steps)
		}
	})
	// Строка нативного резерва, оставшаяся от прежней схемы, не получает
	// актора: ни привязанного к роли, ни постороннего, ни неопределённости.
	t.Run("legacy reserved step", func(t *testing.T) {
		s, bound := setup(t)
		if _, err := s.db.Exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,started_at,request_id,native_actor_name,native_template_name,native_template_uid,native_template_digest) VALUES ('s',1,'actor','inflight','now','req','pipeline-legacy',?,?,?)`, reservation.TemplateName, reservation.TemplateUID, reservation.TemplateDigest); err != nil {
			t.Fatal(err)
		}
		_, before := nativeStep(t, s)
		for _, actor := range []struct {
			uid         string
			incarnation int64
		}{{bound.UID, bound.Incarnation}, {"actor-other", 1}} {
			if err := s.BindPipelineNativeActor(ctx, reservation, actor.uid, actor.incarnation); !errors.Is(err, ErrPipelineNodeActorCreation) {
				t.Fatalf("bind %s: %v, want %v", actor.uid, err, ErrPipelineNodeActorCreation)
			}
		}
		if err := s.MarkPipelineNativeActorUncertain(ctx, reservation); !errors.Is(err, ErrPipelineNodeActorCreation) {
			t.Fatalf("mark uncertain: %v, want %v", err, ErrPipelineNodeActorCreation)
		}
		if got, err := s.ReservePipelineNativeActor(ctx, reservation); !errors.Is(err, ErrPipelineNodeActorCreation) || got.Replay {
			t.Fatalf("replay of a legacy reservation: %+v %v", got, err)
		}
		if steps, after := nativeStep(t, s); steps != 1 || after != before || after != "-@0 inflight pipeline-legacy" {
			t.Fatalf("legacy step changed: %q -> %q (%d steps)", before, after, steps)
		}
	})
}
