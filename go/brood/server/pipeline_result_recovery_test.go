package server

// FEATURE: Реальный startup восстанавливает неизвестные result observations (#290).
// SPEC: Новый Server не повторяет наблюдение, не продвигает граф и не снимает slot;
// fixture — только локальная SQLite история, без provider ресурсов и credentials.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/store"
)

func TestServerStartRecoversPipelineResultObservation(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/brood.db"
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: "actor", Generation: 1, Swarm: "swarm", Step: "delivery", Origin: dispatchOrigin, Commit: dispatchCommit, Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}, Pass: 1, Sequence: 1}
	lease := store.ActorOperation{ProjectID: "p", SwarmID: "swarm", UID: "actor", Generation: 1, OperationID: "observation", Caller: "swarm:swarm", Operation: store.WorkspaceResultOperation, State: "inflight"}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	leaseJSON, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO users VALUES ('u','owner',NULL,'hash',0,0,'now','now')`,
		`INSERT INTO projects VALUES ('p','project','now')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO swarms(id,project_id,owner_id,name,atespace_name,atespace_uid,state,created_at,updated_at,graph,execution,definition_name,definition_version,definition_revision) VALUES('swarm','p','u','swarm','space','native','active','now','now',?,'running','deliver',1,'rev')`, []byte(dispatchGraph)); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO swarm_members VALUES ('swarm','u','owner')`,
		`INSERT INTO pipeline_sources VALUES ('swarm','` + dispatchOrigin + `')`,
		`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,actor_uid,actor_incarnation,request_id,attested_commit,goal,source_commit) VALUES('swarm',1,'build','inflight','actor',1,'delivery','` + dispatchCommit + `','goal','` + dispatchCommit + `')`,
		`INSERT INTO swarm_pass_policies VALUES('swarm',1,'p',NULL,0)`,
		`INSERT INTO pipeline_completion_intents VALUES('swarm',1,1,'delivery','actor',1,'` + dispatchOrigin + `','` + dispatchCommit + `','success','Retained report','now')`,
		`INSERT INTO pipeline_completion_receipts(swarm_id,sequence,version,nonce,created_at) VALUES('swarm',1,1,'` + request.Challenge + `','now')`,
		`INSERT INTO pipeline_completion_acknowledgements VALUES('swarm',1,'now')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES('p','workspace','observation',?,'actor')`, string(leaseJSON)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pipeline_result_observations VALUES('observation','swarm',1,?,?,'inflight','',1)`, string(requestJSON), string(leaseJSON)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	srv, err := New(ctx, Config{DBPath: path}, Options{Provisioner: newFakeProvisioner()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.steps.wait()
	observation, err := srv.store.GetPipelineResultObservation(ctx, "swarm", 1)
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != "unconfirmed" || observation.Revision != 2 {
		t.Fatalf("startup left observation incorrectly live: state=%s revision=%d", observation.State, observation.Revision)
	}
	current, err := srv.store.CurrentActorOperation(ctx, "actor")
	if err != nil {
		t.Fatal(err)
	}
	if current.OperationID != "observation" || current.State != "unconfirmed" {
		t.Fatal("startup released or replaced unknown operation")
	}
	steps, err := srv.store.SwarmSteps(ctx, "swarm")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].State != "inflight" || steps[0].Outcome != nil {
		t.Fatal("startup invented actor result or successor")
	}
}
