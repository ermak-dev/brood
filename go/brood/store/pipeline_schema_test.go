package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

// FEATURE: Persist swarm-scoped pipeline runs and step visits.
// SPEC: Schema 11 preserves version 10 data, opaque graph bytes, ownership and visit state.
// IMPLEMENTATION: Add both tables transactionally and validate their definitions on reopen.
// RESULT: IMPLEMENTED — new and upgraded databases retain run and visit data.
// STATUS: DONE
// BUG: Pipeline runs accept null IDs and can prevent deleting their owner.
// HYPOTHESIS: SQLite permits null text primary keys and the owner foreign key lacks a delete action.
// SOLUTION: Require IDs and cascade owner deletion, matching swarm ownership.
// RESULT: FIXED — null IDs are rejected and deleting an owner removes their runs and visits.
// STATUS: FIXED
func TestPipelineRunIdentityAndOwnerDeletion(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('swarm-owner','owner',NULL,'hash',0,0,'now','now'), ('runner','runner',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','swarm-owner','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("null ID", func(t *testing.T) {
		if _, err := s.db.Exec(`INSERT INTO pipeline_runs (id,swarm_id,owner_id,graph,status) VALUES (NULL,'s','runner',X'01','running')`); err == nil {
			t.Fatal("null pipeline run ID accepted")
		}
	})
	t.Run("delete nonmember owner", func(t *testing.T) {
		if _, err := s.db.Exec(`INSERT INTO pipeline_runs (id,swarm_id,owner_id,graph,status) VALUES ('r','s','runner',X'01','running'); INSERT INTO pipeline_steps (run_id,sequence,node_id,state) VALUES ('r',1,'n','ready')`); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteUser(ctx, "runner"); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		var swarms, runs, steps int
		if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM swarms WHERE id='s'), (SELECT count(*) FROM pipeline_runs WHERE id='r'), (SELECT count(*) FROM pipeline_steps WHERE run_id='r')`).Scan(&swarms, &runs, &steps); err != nil {
			t.Fatal(err)
		}
		if swarms != 1 || runs != 0 || steps != 0 {
			t.Fatalf("swarms = %d, runs = %d, steps = %d; want 1, 0, 0", swarms, runs, steps)
		}
	})
}

func TestPipelineSchema(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "upgrade"}[upgrade], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "store.db")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.db.Exec(`INSERT INTO users VALUES ('u','alice',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'); INSERT INTO swarm_members VALUES ('s','u','owner')`)
			if err != nil {
				t.Fatal(err)
			}
			if upgrade {
				if _, err = s.db.Exec(`DROP TRIGGER pipeline_runs_no_replace; DROP TABLE pipeline_sources; DROP TABLE workflow_revision_counters; DROP TABLE workflow_definitions; DROP TABLE pipeline_steps; DROP TABLE pipeline_runs; PRAGMA user_version=10`); err != nil {
					t.Fatal(err)
				}
				s.Close()
				s, err = Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer s.Close()
			var version, count int
			if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 15 {
				t.Fatalf("version = %d, error = %v", version, err)
			}
			if err = s.db.QueryRow(`SELECT count(*) FROM swarms WHERE id='s'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("swarm count = %d, error = %v", count, err)
			}
			graph := []byte{0, 255, 1}
			_, err = s.db.Exec(`INSERT INTO pipeline_runs (id,swarm_id,owner_id,graph,status) VALUES ('r','s','u',?,'running')`, graph)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.db.Exec(`INSERT INTO pipeline_steps (run_id,sequence,node_id,state,answer,actor_uid,actor_incarnation,request_id) VALUES ('r',1,'n','waiting','answer','actor',2,'request'),('r',2,'n','done','','actor',2,'request2')`)
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			if err = s.db.QueryRow(`SELECT graph FROM pipeline_runs WHERE id='r'`).Scan(&got); err != nil || !bytes.Equal(got, graph) {
				t.Fatalf("graph = %v, error = %v", got, err)
			}
			if err = s.db.QueryRow(`SELECT count(*) FROM pipeline_steps WHERE run_id='r' AND node_id='n'`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("visits = %d, error = %v", count, err)
			}
			if _, err = s.db.Exec(`INSERT INTO pipeline_steps (run_id,sequence,node_id,state) VALUES ('missing',1,'n','ready')`); err == nil {
				t.Fatal("orphan step accepted")
			}
			if _, err = s.db.Exec(`DELETE FROM swarms WHERE id='s'`); err != nil {
				t.Fatal(err)
			}
			if err = s.db.QueryRow(`SELECT count(*) FROM pipeline_steps`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("orphan visits = %d, error = %v", count, err)
			}
		})
	}
}
