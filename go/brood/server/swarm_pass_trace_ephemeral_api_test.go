package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Human trace экспортирует queued ephemeral job отдельными сохранёнными фактами (#292).
// SPEC: Нет allocation/source/result из догадки, приватных provider/root полей или эффектов.
// IMPLEMENTATION: Явные DTO разделяют очередь, резерв, proof, inventory и retained report.
// RESULT: IMPLEMENTED — полный пул не теряет queued job, отсутствие inventory не выдаётся за deletion.
// STATUS: DONE
func traceEphemeralJob(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	steps, ok := body["steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Fatal("trace lost its parent step")
	}
	jobs, ok := steps[0].(map[string]any)["ephemeralJobs"].([]any)
	if !ok || len(jobs) != 1 {
		t.Fatalf("human trace omitted queued/retained ephemeral job: %+v", steps[0])
	}
	return jobs[0].(map[string]any)
}

func TestPassTraceAPIDistinguishesReservedCreateFromSourceProof(t *testing.T) {
	ctx := context.Background()
	d, intent, reservation := ephemeralCapacityFixture(t)
	allocation, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.ReserveEphemeralChildCreate(ctx, intent.ID, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	before, messages := d.calls()
	response, body, _ := d.do(d.alice, "GET", "/api/swarms/"+d.swarm+"/passes/1", "", nil)
	expectStatus(t, "allocated pending proof", response, body, http.StatusOK)
	job := traceEphemeralJob(t, body)
	assignment := job["allocation"].(map[string]any)
	inventory := job["inventory"].(map[string]any)
	grant := job["createAuthorization"].(map[string]any)
	if assignment["actorUid"] != allocation.ActorUID || assignment["actorIncarnation"] != float64(allocation.ActorIncarnation) || inventory["phase"] != "provisioning" || inventory["operation"] != "create" || grant["state"] != "reserved" || grant["reservedAt"] == nil {
		t.Fatalf("allocated facts were rewritten: %+v", job)
	}
	for _, absent := range []string{"attestation", "childId", "resultRetention", "state"} {
		if job[absent] != nil {
			t.Fatalf("may-have-issued create fabricated %s", absent)
		}
	}
	after, sent := d.calls()
	if len(before) != len(after) || len(messages) != len(sent) {
		t.Fatal("allocated history triggered execution")
	}
}

func TestPassTraceAPIKeepsRetainedEphemeralFactsWithoutInventory(t *testing.T) {
	ctx := context.Background()
	d, actor, child := runningEphemeralChildReportFixture(t)
	d.srv.steps.close()
	d.srv.actorDelivery.close()
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := d.srv.store.GetEphemeralChildIntentForTask(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	peer := childPrincipal(d.worker, work.Parent.Principal.ProjectID)
	other, err := d.srv.store.ReserveSwarmChild(ctx, work.Parent, "other-report", "The other declared child", peer, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	other, err = d.srv.store.StartSwarmChild(ctx, work.Parent, other.RequestID, peer, other.Revision, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.CompleteSwarmChild(ctx, work.Parent, other.RequestID, peer, other.Revision, "success", "Other independent report", d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	report := "Original ephemeral report retained outside the workspace"
	if err := d.srv.managed.completeChild(ctx, actor, child.ID, child.RequestID, "success", report); err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.JoinSwarmChildren(ctx, work.Parent, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	if err := d.srv.store.SealEphemeralChildResult(ctx, intent.ID, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Только отсутствие inventory в тестовой базе, не PVE cleanup acceptance.
	_, err = db.Exec(`DELETE FROM managed_actors WHERE uid=?`, actor.UID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, messages := d.calls()
	response, body, raw := d.do(d.alice, "GET", "/api/swarms/"+d.swarm+"/passes/1", "", nil)
	expectStatus(t, "retained ephemeral facts", response, body, http.StatusOK)
	job := traceEphemeralJob(t, body)
	proof := job["attestation"].(map[string]any)
	retained := job["resultRetention"].(map[string]any)
	if job["childId"] != child.ID || job["inventory"] != nil || job["inventoryUnavailable"] != "not_retained" || proof["state"] != "attested" || proof["verifiedCommit"] != intent.SourceCommit || retained["childId"] != child.ID || retained["childRevision"] != float64(child.Revision+1) || retained["outcome"] != "success" || retained["parentOutcome"] != "success" || retained["retainedAt"] == nil || job["state"] != nil {
		t.Fatalf("historical facts lost or inventory absence became deletion proof: %+v", job)
	}
	children := body["steps"].([]any)[0].(map[string]any)["children"].(map[string]any)["children"].([]any)
	found := false
	for _, item := range children {
		row := item.(map[string]any)
		if row["id"] == child.ID {
			found = row["report"] == report && row["outcome"] == "success"
		}
	}
	if !found {
		t.Fatal("job metadata replaced or lost independent report")
	}
	for _, forbidden := range []string{"reportHash", "parentReportHash", "lease", "Lease", "OwnerNonce", "ownerNonce", "signature", "Signature", "journalId", "JournalID"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatalf("private internal evidence exported: %s", forbidden)
		}
	}
	after, sent := d.calls()
	if len(before) != len(after) || len(messages) != len(sent) {
		t.Fatal("historical read triggered guest work")
	}
}

func TestPassTraceAPIExportsQueuedEphemeralJobWithoutAllocating(t *testing.T) {
	ctx := context.Background()
	d, intent, _ := ephemeralCapacityFixture(t)
	pool, err := d.srv.store.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	pool.MaxActors = 2
	if _, err := d.srv.store.UpdateManagedPool(ctx, pool, pool.Revision); err != nil {
		t.Fatal(err)
	}
	before, sent := d.calls()
	path := "/api/swarms/" + d.swarm + "/passes/1"
	response, body, raw := d.do(d.alice, "GET", path, "", nil)
	expectStatus(t, "queued ephemeral trace", response, body, http.StatusOK)
	job := traceEphemeralJob(t, body)
	if job["id"] != intent.ID || job["requestId"] != intent.RequestID || job["task"] != intent.Payload || job["poolUid"] != intent.PoolUID || job["queuedAt"] != intent.CreatedAt {
		t.Fatalf("queued job identity or source bytes changed: %+v", job)
	}
	source, ok := job["source"].(map[string]any)
	if !ok || source["origin"] != intent.Origin || source["commit"] != intent.SourceCommit {
		t.Fatalf("job expected source lost: %+v", job)
	}
	for _, absent := range []string{"allocation", "inventory", "attestation", "childId", "createAuthorization", "resultRetention"} {
		if job[absent] != nil {
			t.Fatalf("unallocated job fabricated %s", absent)
		}
	}
	for _, forbidden := range []string{"OwnerNonce", "ownerNonce", "CTID", "ctid", "UPID", "upid", "lease", "Lease", "signature", "Signature", "challenge", "Challenge", "journalId", "JournalID", "CreateOperationID"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatalf("private field exported: %s", forbidden)
		}
	}
	response, download, _ := d.do(d.alice, "GET", path+"?download=1", "", nil)
	expectStatus(t, "queued job download", response, download, http.StatusOK)
	if !reflect.DeepEqual(download, body) {
		t.Fatal("download changed queued work")
	}
	bob := newClient(t)
	d.login(bob, "bob", "bob-password-1")
	response, hidden, _ := d.do(bob, "GET", path, "", nil)
	expectStatus(t, "hidden queued task", response, hidden, http.StatusNotFound)
	if _, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("history read allocated a worker: %v", err)
	}
	after, messages := d.calls()
	if len(before) != len(after) || len(sent) != len(messages) {
		t.Fatal("history read attested or delivered work")
	}
}
