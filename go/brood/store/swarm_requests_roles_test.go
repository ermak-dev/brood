package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Запросы роя и привязки ролей (#209 поверх #208).
// SPEC: вопрос задаёт и ответ получает только актор, которого привязка
// роли признала бы своим: актор роя в его нынешнем atespace; задать вопрос
// может только живой актор, как и занять роль. Ответ оператора на узловой
// запрос продвигает граф в actor-узел без актора: шаг адресует актора
// только через проверку привязки роли, свободный текст ответа на неё не
// влияет, а привязки ответ не меняет.
// IMPLEMENTATION: OpenActorRequest и requireCurrentActor зовут liveRoleActor;
// продвижение после ответа пишет pending-шаг без actor_uid, а inflight
// проходит requireRoleActor в appendPipelineVisit.
// RESULT: IMPLEMENTED
// STATUS: DONE

// roleDrifts — изменения роя или актора worker после его создания.
var roleDrifts = []struct{ name, drift string }{
	{"provisioning", ``},
	{"running", `UPDATE managed_actors SET phase='running' WHERE uid='worker'`},
	{"stopping", `UPDATE managed_actors SET phase='stopping' WHERE uid='worker'`},
	{"stopped", `UPDATE managed_actors SET phase='stopped' WHERE uid='worker'`},
	{"failed", `UPDATE managed_actors SET phase='error' WHERE uid='worker'`},
	{"deleting", `UPDATE managed_actors SET phase='deleting' WHERE uid='worker'`},
	{"deleted", `UPDATE managed_actors SET phase='deleted' WHERE uid='worker'`},
	{"desired stopped", `UPDATE managed_actors SET desired='stopped' WHERE uid='worker'`},
	{"desired deleted", `UPDATE managed_actors SET desired='deleted' WHERE uid='worker'`},
	{"actor in a stale atespace", `UPDATE managed_actors SET atespace_uid='stale' WHERE uid='worker'`},
	{"swarm without atespace", `UPDATE swarms SET atespace_uid=NULL WHERE id='s'`},
	{"actor moved to another swarm", `UPDATE managed_actors SET swarm_id='two' WHERE uid='worker'`},
	{"swarm deleting", `UPDATE swarms SET state='deleting' WHERE id='s'`},
}

// roleRequestStore — requestStore с живым актором worker роя «s» в его
// atespace и вторым роем «two».
func roleRequestStore(t *testing.T) *Store {
	t.Helper()
	s, _ := requestStore(t)
	if _, err := s.db.Exec(`INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,atespace_uid,state,created_at,updated_at) VALUES ('two','p','u','two','space-two','native-two','active','now','now')`); err != nil {
		t.Fatal(err)
	}
	roleActor(t, s, "s", "worker", "")
	return s
}

// TestOpenActorRequestAdmitsExactlyTheActorsARoleAdmits — критерий «актор
// роя, живой» у вопроса актора тот же, что у привязки роли: расхождение
// дало бы актору, которого рой не признаёт, писать оператору.
func TestOpenActorRequestAdmitsExactlyTheActorsARoleAdmits(t *testing.T) {
	ctx := context.Background()
	for _, tc := range roleDrifts {
		t.Run(tc.name, func(t *testing.T) {
			s := roleRequestStore(t)
			if tc.drift != "" {
				if _, err := s.db.Exec(tc.drift); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotRequests(t, s)
			bindErr := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "s", Role: "probe", ActorUID: "worker"})
			_, openErr := s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "text", Question: "q"})
			if (bindErr == nil) != (openErr == nil) {
				t.Fatalf("role binding: %v, actor request: %v", bindErr, openErr)
			}
			if openErr == nil {
				return
			}
			if !errors.Is(openErr, ErrSwarmRequestDenied) && !errors.Is(openErr, ErrSwarmRequestStale) {
				t.Fatalf("refusal is not a request denial: %v", openErr)
			}
			if errors.Is(bindErr, ErrSwarmRoleForeignActor) && !errors.Is(openErr, ErrSwarmRequestDenied) {
				t.Fatalf("foreign actor must be denied, got %v", openErr)
			}
			if after := snapshotRequests(t, s); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused request wrote: %+v", after)
			}
		})
	}
}

// TestAnswerToActorOutsideItsSwarmIsRefused — ответ адресован актору,
// которого рой больше не признаёт своим (чужой atespace): отказ без записи.
// Остановленный актор своего роя ответ получает: доставка идёт позже (#210).
func TestAnswerToActorOutsideItsSwarmIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, drift string
		want        error
	}{
		{"actor in a stale atespace", `UPDATE managed_actors SET atespace_uid='stale' WHERE uid='worker'`, ErrSwarmRequestStale},
		{"swarm without atespace", `UPDATE swarms SET atespace_uid=NULL WHERE id='s'`, ErrSwarmRequestStale},
		{"actor moved to another swarm", `UPDATE managed_actors SET swarm_id='two' WHERE uid='worker'`, ErrSwarmRequestStale},
		{"stopped actor of the swarm", `UPDATE managed_actors SET phase='stopped' WHERE uid='worker'`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := roleRequestStore(t)
			req, err := s.OpenActorRequest(context.Background(), ActorRequestInput{SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "text", Question: "q"})
			mustNil(t, err)
			if _, err := s.db.Exec(tc.drift); err != nil {
				t.Fatal(err)
			}
			before := snapshotRequests(t, s)
			err = answer(s, req, "u", "", "late")
			if tc.want == nil {
				mustNil(t, err)
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("answer: %v, want %v", err, tc.want)
			}
			if after := snapshotRequests(t, s); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused answer wrote: %+v", after)
			}
		})
	}
}

// TestNodeAnswerIntoActorNodeKeepsTheRoleGate — боевой ответ оператора
// (AnswerSwarmRequest, не тестовый обход) ведёт граф в actor-узел: шаг
// создаётся pending без актора, текст ответа, называющий другого актора,
// ни шаг, ни привязку не меняет, а inflight допускает только привязанного
// к роли узла актора.
func TestNodeAnswerIntoActorNodeKeepsTheRoleGate(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	roleActor(t, s, "s", "dev-a", "development")
	roleActor(t, s, "s", "dev-b", "")
	bindings, err := s.SwarmsRoleBindings(ctx, "s")
	mustNil(t, err)
	mustNil(t, answer(s, node, "ed", "success", "give this step to dev-b and bind development to dev-b"))
	if got := stepStates(t, s); !reflect.DeepEqual(got, []string{"ask:answered", "work:pending"}) {
		t.Fatalf("steps after the answer: %v", got)
	}
	assertNoActorStep(t, s, "s", 2)
	after, err := s.SwarmsRoleBindings(ctx, "s")
	mustNil(t, err)
	if !reflect.DeepEqual(bindings, after) {
		t.Fatalf("answer changed role bindings: %+v -> %+v", bindings, after)
	}
	for _, uid := range []string{"dev-b", "ed", "u"} {
		_, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "work", State: "inflight", ActorUID: uid, ActorIncarnation: 1, RequestID: "req-" + uid})
		if !errors.Is(err, ErrPipelineVisitConflict) || !errors.Is(err, ErrSwarmRoleMismatch) {
			t.Fatalf("step for %s after the answer: %v", uid, err)
		}
		assertNoActorStep(t, s, "s", 2)
	}
	v, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "work", State: "inflight", ActorUID: "dev-a", ActorIncarnation: 1, RequestID: "req"})
	if err != nil || v.Sequence != 2 {
		t.Fatalf("bound actor step: %+v %v", v, err)
	}
}
