package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: Scoped ответ читается только пока живёт исходная child-задача (#292).
// SPEC: Retained pass context и proof-bound running child обязательны;
// read-only проверка не является fresh delivery permission и не меняет историю.
// IMPLEMENTATION: Answered request, исходный pass context и running child в одном snapshot.
// RESULT: IMPLEMENTED — поздний ответ и stale scope отказывают без записи; full/race зелёные.
// STATUS: DONE
func TestEphemeralAnswerChecksExactRunningChildWithoutEffects(t *testing.T) {
	ctx := context.Background()
	s, intent, child, in := ephemeralQuestionFixture(t)
	q, err := s.OpenEphemeralChildRequest(ctx, child.ID, in, childAllow)
	mustNil(t, err)
	if err := s.CheckEphemeralChildAnswer(ctx, q.SwarmID, q.ID, childAllow); err == nil {
		t.Fatal("unanswered question authorized as an answer")
	}
	mustNil(t, answer(s, q, "u", "", "Use the isolated fixture."))
	before := databaseRows(t, s.db)
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		if a.Action != "deliver_ephemeral_answer" || a.Caller != child.Executor || a.Executor != child.Executor || a.Parent != intent.Parent {
			t.Fatalf("answer lost original child authority: %+v", a)
		}
		_, err := s.GetManagedActor(ctx, child.Executor.ActorUID)
		return err
	}
	mustNil(t, s.CheckEphemeralChildAnswer(ctx, q.SwarmID, q.ID, auth))
	if calls != 1 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("answer check changed state or skipped current authority")
	}
	_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision, "success", "Independent result, not the human answer.", childAllow)
	mustNil(t, err)
	before = databaseRows(t, s.db)
	if err := s.CheckEphemeralChildAnswer(ctx, q.SwarmID, q.ID, auth); err == nil {
		t.Fatal("late answer authorized after child completion")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("late answer check changed completed work")
	}
}

func TestEphemeralAnswerRefusesMissingChangedOrForeignContext(t *testing.T) {
	for _, change := range []string{"missing-context", "changed-context", "wrong-swarm", "wrong-request", "nil", "denied", "incarnation", "owner", "proof"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, _, child, in := ephemeralQuestionFixture(t)
			q, err := s.OpenEphemeralChildRequest(ctx, child.ID, in, childAllow)
			mustNil(t, err)
			mustNil(t, answer(s, q, "u", "", "Isolated fixture answer."))
			swarm, request, auth := q.SwarmID, q.ID, SwarmChildAuthorize(childAllow)
			switch change {
			case "missing-context":
				_, err = s.db.Exec(`DROP TRIGGER swarm_request_pass_contexts_no_delete; DELETE FROM swarm_request_pass_contexts WHERE request_id=?`, q.ID)
				mustNil(t, err)
			case "changed-context":
				_, err = s.db.Exec(`DROP TRIGGER swarm_request_pass_contexts_no_update; UPDATE swarm_request_pass_contexts SET observed_sequence=observed_sequence+1 WHERE request_id=?`, q.ID)
				mustNil(t, err)
			case "wrong-swarm":
				swarm = "other"
			case "wrong-request":
				request = "other"
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "incarnation":
				setActor(t, s, child.Executor.ActorUID, "incarnation=2")
			case "owner":
				p := child.Executor
				_, err = s.db.Exec(`INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES('answer-owner',?,?,?,?,'u','operator:u','session_terminal','completed','now')`, p.ProjectID, p.SwarmID, p.ActorUID, p.Incarnation)
				mustNil(t, err)
			case "proof":
				_, err = s.db.Exec(`DROP TRIGGER ephemeral_child_attestation_update_guard; UPDATE ephemeral_child_attestations SET state='unconfirmed',attested_origin='',attested_commit=''`)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			if err := s.CheckEphemeralChildAnswer(ctx, swarm, request, auth); err == nil {
				t.Fatal("stale/foreign child context authorized answer")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refused answer check changed state")
			}
		})
	}
}
