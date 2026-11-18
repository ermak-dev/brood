package store

// FEATURE: Receipt migration не выдумывает получение старых acceptance (#290).
// SPEC: Непустая unacknowledged observation schema40 сохраняется byte-for-byte;
// новые таблицы пусты, новый RPC запрещён, recovery оставляет unknown без ACK.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
)

func seedUnacknowledgedResultV40(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO swarms(id,project_id,owner_id,name,atespace_name,atespace_uid,state,created_at,updated_at,graph,execution,definition_name,definition_version,definition_revision) VALUES('legacy-result','p','u','legacy-result','space-result','native-result','active','now','now',?,'running','build-first',1,'fixture')`, []byte(startBuildFirst))
	mustNil(t, err)
	for _, query := range []string{
		`INSERT INTO swarm_members VALUES('legacy-result','u','owner')`,
		`INSERT INTO pipeline_sources VALUES('legacy-result','` + startOrigin + `')`,
		`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,actor_uid,actor_incarnation,request_id,attested_commit,goal,source_commit) VALUES('legacy-result',1,'build','inflight','legacy-result-actor',1,'legacy-delivery','` + startCommit + `','legacy result goal','` + startCommit + `')`,
		`INSERT INTO swarm_pass_policies VALUES('legacy-result',1,'p',NULL,0)`,
		`INSERT INTO pipeline_completion_intents VALUES('legacy-result',1,1,'legacy-delivery','legacy-result-actor',1,'` + startOrigin + `','` + startCommit + `','success','Retained result report','now')`,
	} {
		_, err := db.Exec(query)
		mustNil(t, err)
	}
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: "legacy-result-actor", Generation: 1, Swarm: "legacy-result", Step: "legacy-delivery", Origin: startOrigin, Commit: startCommit, Challenge: completionNonce()}, Pass: 1, Sequence: 1}
	raw, err := json.Marshal(request)
	mustNil(t, err)
	lease := ActorOperation{ProjectID: "p", SwarmID: "legacy-result", UID: "legacy-result-actor", Generation: 1, OperationID: "legacy-observation", Caller: "swarm:legacy-result", Operation: WorkspaceResultOperation, State: "inflight"}
	_, err = db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES('p','workspace','legacy-observation',?,'legacy-result-actor')`, encodeManaged(lease))
	mustNil(t, err)
	_, err = db.Exec(`INSERT INTO pipeline_result_observations VALUES('legacy-observation','legacy-result',1,?,?,'inflight','',1)`, string(raw), encodeManaged(lease))
	mustNil(t, err)
}

func TestCompletionReceiptMigrationPreservesUnacknowledgedLegacyObservation(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 40)
	raw := rawDatabase(t, path)
	seedUnacknowledgedResultV40(t, raw)
	before := withEmptyDeliveryAndRequestContextTables(databaseRows(t, raw))
	before["pipeline_completion_receipts"] = nil
	before["pipeline_completion_acknowledgements"] = nil
	before["pipeline_policy_candidates"] = nil
	before["pipeline_policy_confirmations"] = nil
	mustNil(t, raw.Close())
	for range 2 {
		s, err := Open(ctx, path)
		mustNil(t, err)
		if !reflect.DeepEqual(databaseRows(t, s.db), before) {
			t.Fatal("receipt migration changed history or invented client acknowledgement")
		}
		observation, err := s.GetPipelineResultObservation(ctx, "legacy-result", 1)
		mustNil(t, err)
		if err := s.CheckPipelineResultObservation(ctx, observation); !errors.Is(err, ErrPipelineCompletionReceiptPending) {
			t.Fatalf("legacy observation inferred client receipt: %v", err)
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
	s, err := Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	mustNil(t, s.RecoverPipelineResultObservations(ctx))
	observation, err := s.GetPipelineResultObservation(ctx, "legacy-result", 1)
	mustNil(t, err)
	if observation.State != "unconfirmed" {
		t.Fatal("legacy external effect was retried or forgotten")
	}
	if _, err := s.GetPipelineCompletionReceipt(ctx, "legacy-result", 1); !errors.Is(err, ErrNotFound) {
		t.Fatal("recovery invented receipt")
	}
}
