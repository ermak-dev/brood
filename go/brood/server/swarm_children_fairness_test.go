package server

// FEATURE: Bounded child scheduling remains fair beyond one scan window (#289).
// SPEC: Stale executor/parent heads are retained, not discarded, but 128 such
// heads must not permanently starve a later valid allocation in another swarm.
// IMPLEMENTATION: Private rotating keyset cursor, without dropping stale work.
// RESULT: IMPLEMENTED — full Go suite and focused race checks pass.
// STATUS: DONE

import (
	"context"
	"fmt"
	"testing"

	"brood.dev/go/brood/store"
)

func TestChildExecutionFairBeyondStaleScanWindow(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	s := d.srv.store
	original, err := s.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	// Historical intent can outlive a parent or executor. Seed two complete
	// bounded groups with independently stale identities, without provisioning.
	for group := range 2 {
		id := fmt.Sprintf("stale-swarm-%d", group)
		uid := "native-" + id
		parent := "missing-parent-" + id
		if err := s.CreateSwarm(ctx, store.Swarm{ID: id, ProjectID: original.ProjectID, OwnerID: original.OwnerID, Name: id, AtespaceName: "space-" + id, AtespaceUID: &uid, State: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := insertSwarmGraph(ctx, d.testServer, id, []byte(dispatchGraph), "running"); err != nil {
			t.Fatal(err)
		}
		d.exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,actor_uid,actor_incarnation,request_id,attested_commit,goal,source_commit) VALUES(?,1,'build','inflight',?,1,'parent',?,'Historical pass',?)`, id, parent, dispatchCommit, dispatchCommit)
		d.exec(`INSERT INTO swarm_child_joins(swarm_id,parent_sequence,pass,project_id,atespace_uid,parent_uid,parent_incarnation,parent_request_id,mode,total,quorum,remaining,created_at) VALUES(?,1,1,?,?,?,1,'parent','all',64,0,'retain','now')`, id, original.ProjectID, uid, parent)
		for i := range 64 {
			child := fmt.Sprintf("stale-%d-%d", group, i)
			d.exec(`INSERT INTO swarm_child_tasks(id,swarm_id,parent_sequence,request_id,payload,actor_uid,actor_incarnation,state,revision,created_at,role,delivery) VALUES(?,?,1,?,'Historical task',?,1,'pending',1,'now','review','queued')`, child, id, child, "missing-executor-"+child)
		}
	}
	args := childArgs(d, "live")
	args.Total = 1
	if _, err := d.srv.managed.createChild(ctx, d.master, args); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		d.srv.children.dispatch(ctx)
	}
	d.mu.Lock()
	count := len(d.messages)
	d.mu.Unlock()
	if count != 2 {
		t.Fatalf("stale scan heads starved later worker: %d messages, want parent plus one live child", count)
	}
}
