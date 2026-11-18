package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Вопрос ephemeral исполнителя привязан к его running child (#292).
// SPEC: Только retained proof и точные parent/pass/child/UID/inc, без global fallback;
// вопрос и его pass context атомарны, ответ не завершает child или graph visit.
// IMPLEMENTATION: Отдельный scoped API и проверка записи точного immutable context.
// RESULT: IMPLEMENTED — scoped/legacy/race и полный Go build/test/vet прошли.
// STATUS: DONE
func ephemeralQuestionFixture(t *testing.T) (*Store, EphemeralChildIntent, SwarmChildTask, ActorRequestInput) {
	t.Helper()
	s, intent, child, delivery := childDeliveryFixture(t)
	_, err := s.PrepareEphemeralChildDelivery(context.Background(), intent.ID, delivery, childAllow)
	mustNil(t, err)
	in := ActorRequestInput{SwarmID: child.Executor.SwarmID, ActorUID: child.Executor.ActorUID, ActorIncarnation: child.Executor.Incarnation, Kind: "text", Question: "Which target should this child use?", Context: "Exact child context, not a graph decision."}
	child.State, child.Revision = "running", child.Revision+1
	return s, intent, child, in
}

func TestEphemeralQuestionRetainsExactContextWithoutCompletingWork(t *testing.T) {
	ctx := context.Background()
	s, intent, child, in := ephemeralQuestionFixture(t)
	before, err := s.SwarmSteps(ctx, in.SwarmID)
	mustNil(t, err)
	calls := 0
	authorize := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		if a.Action != "ask_ephemeral_operator" || a.Caller != child.Executor || a.Executor != child.Executor || a.Parent != intent.Parent {
			t.Fatalf("question lost authenticated child scope: %+v", a)
		}
		_, err := s.GetManagedActor(ctx, in.ActorUID)
		return err
	}
	q, err := s.OpenEphemeralChildRequest(ctx, child.ID, in, authorize)
	mustNil(t, err)
	if calls != 1 || q.Origin != "actor" || q.ActorUID != in.ActorUID || q.ActorIncarnation != in.ActorIncarnation || q.Sequence != nil || q.Outcome != nil || q.Question != in.Question || q.Context != in.Context || q.State != "open" {
		t.Fatalf("question fabricated graph authority or changed input: %+v", q)
	}
	retained, err := s.GetSwarmRequestPassContext(ctx, q.SwarmID, q.ID)
	mustNil(t, err)
	if retained != (SwarmRequestPassContext{SwarmID: in.SwarmID, RequestID: q.ID, Pass: intent.Parent.Pass, ObservedSequence: intent.Parent.Sequence}) {
		t.Fatalf("question context was guessed from another visit: %+v", retained)
	}
	mustNil(t, answer(s, q, "u", "", "Use the isolated fixture target."))
	after, err := s.SwarmSteps(ctx, in.SwarmID)
	mustNil(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("human answer advanced the graph")
	}
	work, err := s.SwarmChildWorkByID(ctx, child.ID)
	mustNil(t, err)
	if work.Task != child {
		t.Fatal("human answer completed or modified independent child work")
	}
	if _, err := s.OpenActorRequest(ctx, in); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("global request path admitted ephemeral actor: %v", err)
	}
}

func TestEphemeralQuestionRefusesStaleUnprovedAndForeignScope(t *testing.T) {
	for _, change := range []string{"empty-child", "foreign-child", "wrong-actor", "wrong-inc", "wrong-swarm", "pending", "completed", "proof", "parent-inc", "parent-owner", "executor-owner", "nil", "denied"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, intent, child, in := ephemeralQuestionFixture(t)
			id := child.ID
			auth := SwarmChildAuthorize(childAllow)
			switch change {
			case "empty-child":
				id = ""
			case "foreign-child":
				id = "foreign"
			case "wrong-actor":
				in.ActorUID = intent.Parent.Principal.ActorUID
			case "wrong-inc":
				in.ActorIncarnation++
			case "wrong-swarm":
				in.SwarmID = "two"
			case "pending":
				_, err := s.db.Exec(`DROP TRIGGER swarm_child_tasks_fence; UPDATE swarm_child_tasks SET state='pending',revision=revision+1 WHERE id=?`, child.ID)
				mustNil(t, err)
			case "completed":
				_, err := s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision, "success", "Independent retained report.", childAllow)
				mustNil(t, err)
			case "proof":
				_, err := s.db.Exec(`DROP TRIGGER ephemeral_child_attestation_update_guard; UPDATE ephemeral_child_attestations SET state='unconfirmed',attested_origin='',attested_commit=''`)
				mustNil(t, err)
			case "parent-inc":
				setActor(t, s, intent.Parent.Principal.ActorUID, "incarnation=2")
			case "parent-owner", "executor-owner":
				p := child.Executor
				if change == "parent-owner" {
					p = intent.Parent.Principal
				}
				_, err := s.db.Exec(`INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES('question-owner',?,?,?,?,'u','operator:u','session_terminal','completed','now')`, p.ProjectID, p.SwarmID, p.ActorUID, p.Incarnation)
				mustNil(t, err)
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			}
			before := databaseRows(t, s.db)
			q, err := s.OpenEphemeralChildRequest(ctx, id, in, auth)
			if err == nil || q.ID != "" {
				t.Fatalf("invalid child question accepted: %+v %v", q, err)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refused child scope wrote question or notification")
			}
		})
	}
}

func TestEphemeralQuestionContextFailureRollsEverythingBack(t *testing.T) {
	for _, trigger := range []string{"RAISE(ABORT,'test context refusal')", "RAISE(IGNORE)"} {
		t.Run(trigger, func(t *testing.T) {
			s, _, child, in := ephemeralQuestionFixture(t)
			_, err := s.db.Exec(`CREATE TRIGGER refuse_child_question_context BEFORE INSERT ON swarm_request_pass_contexts BEGIN SELECT ` + trigger + `; END`)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			if q, err := s.OpenEphemeralChildRequest(context.Background(), child.ID, in, childAllow); err == nil || q.ID != "" {
				t.Fatal("question acknowledged without exact child pass context")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("failed context retained orphan question or notification")
			}
		})
	}
}
