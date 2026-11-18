package store

import (
	"context"
	"strings"
	"testing"
)

func TestSwarmChildrenSchemaExists(t *testing.T) {
	s := newTestStore(t)
	for _, table := range []string{"swarm_child_joins", "swarm_child_tasks"} {
		var found bool
		mustNil(t, s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&found))
		if !found {
			t.Errorf("durable child storage is missing table %s", table)
		}
	}
}

func TestSwarmChildrenParentFences(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	attestOne(t, s, "parent")
	_, err := s.db.Exec(`INSERT INTO swarm_child_joins(swarm_id,parent_sequence,pass,project_id,atespace_uid,parent_uid,parent_incarnation,parent_request_id,mode,total,quorum,remaining,created_at) VALUES('one',1,1,'one','native-one','a',1,'parent','any',2,0,'retain','now')`)
	mustNil(t, err)
	for _, statement := range []string{
		`UPDATE swarm_steps SET state='completed',outcome='success' WHERE swarm_id='one' AND sequence=1`,
		`UPDATE swarm_steps SET actor_incarnation=2 WHERE swarm_id='one' AND sequence=1`,
		`DELETE FROM swarm_steps WHERE swarm_id='one' AND sequence=1`,
		`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES('one',2,'done','completed')`,
		`UPDATE swarms SET execution='stopped' WHERE id='one'`,
		`UPDATE swarm_child_joins SET total=1 WHERE swarm_id='one'`,
		`DELETE FROM swarm_child_joins WHERE swarm_id='one'`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Errorf("child fence accepted %s", statement)
		}
	}
	if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", ""); err == nil || !strings.Contains(err.Error(), "child") {
		t.Fatalf("parent completion with unregistered children: %v", err)
	}
	_, err = s.StopSwarm(ctx, "one", "u", allow)
	mustNil(t, err)
	if step := stepOf(t, s, 1); step.State != "unconfirmed" || step.Outcome != nil || step.FinishedAt != nil {
		t.Fatalf("owner stop falsely closed parent: %+v", step)
	}
	var total int
	mustNil(t, s.db.QueryRow(`SELECT total FROM swarm_child_joins WHERE swarm_id='one' AND parent_sequence=1`).Scan(&total))
	if total != 2 {
		t.Fatalf("owner stop lost immutable missing-slot obligation: %d", total)
	}
	if _, err := s.db.Exec(`UPDATE swarms SET execution='succeeded' WHERE id='one'`); err == nil {
		t.Fatal("owner stop journal incorrectly authorizes successful closure")
	}
}
