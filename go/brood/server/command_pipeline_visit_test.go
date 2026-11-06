package server

// FEATURE: Authenticated guests report pipeline actor visit outcomes.
// SPEC: Only the exact current actor and swarm may complete a matching visit;
// reject forged fields, stale identities, unrelated runs and conflicting retries.
// IMPLEMENTATION: Resolve relay identity and complete within one transaction.
// RESULT: IMPLEMENTED — scoped completion and exact retries are enforced.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"brood.dev/go/brood/app"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

const actorVisitGraph = `apiVersion: brood.dev/v1alpha1
name: test
start: actor
exit: done
nodes:
  actor: {type: actor, template: development, prompt: work}
  done: {type: exit}
edges:
  - {from: actor, to: done}
goalGates: [done]
`

// BUG: Namespace lookup holds the only database connection during completion.
// HYPOTHESIS: External authorization is invoked inside the completion transaction.
// SOLUTION: Check the namespace first, then reauthorize durable bindings in the transaction.
// RESULT: Verification blocked by an unrelated store build failure.
// STATUS: PENDING
type probingProvisioner struct {
	app.Provisioner
	probe func() error
	uid   string
}

func (p *probingProvisioner) GetAtespace(string) (string, error) {
	if err := p.probe(); err != nil {
		return "", err
	}
	return p.uid, nil
}

func TestPipelineCompletionNamespaceOutsideTransaction(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	ctx := context.Background()
	s := ts.srv.store
	if err := s.CreatePipelineRun(ctx, store.PipelineRun{ID: "probe-run", SwarmID: master.SwarmID, OwnerID: master.DelegatorID, Graph: []byte(actorVisitGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	visit, err := s.AppendPipelineVisit(ctx, master.SwarmID, "probe-run", store.PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: worker.UID, ActorIncarnation: worker.Incarnation, RequestID: "req"})
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.ateapi = nil
	ts.srv.provision = &probingProvisioner{uid: worker.AtespaceUID, probe: func() error {
		probeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := s.GetSwarm(probeCtx, master.SwarmID)
		return err
	}}
	inv := coordinationInvocation("pipeline_visit_complete", map[string]any{"run_id": "probe-run", "sequence": visit.Sequence, "request_id": "req", "outcome": "success"})
	if r := ts.srv.managed.handleActorCommand(ctx, worker, inv); !r.OK {
		t.Fatalf("namespace lookup blocked database access: %+v", r)
	}
}

func TestPipelineCompletionReauthorizesAfterNamespaceLookup(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	ctx := context.Background()
	s := ts.srv.store
	if err := s.CreatePipelineRun(ctx, store.PipelineRun{ID: "race-run", SwarmID: master.SwarmID, OwnerID: master.DelegatorID, Graph: []byte(actorVisitGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	visit, err := s.AppendPipelineVisit(ctx, master.SwarmID, "race-run", store.PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: worker.UID, ActorIncarnation: worker.Incarnation, RequestID: "req"})
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.ateapi = nil
	ts.srv.provision = &probingProvisioner{uid: worker.AtespaceUID, probe: func() error {
		worker.Phase = "stopped"
		_, err := s.UpdateManagedActor(ctx, worker, worker.Revision)
		return err
	}}
	inv := coordinationInvocation("pipeline_visit_complete", map[string]any{"run_id": "race-run", "sequence": visit.Sequence, "request_id": "req", "outcome": "success"})
	r := ts.srv.managed.handleActorCommand(ctx, worker, inv)
	if r.OK || r.Text != "pipeline completion not authorized" {
		t.Fatalf("revoked actor response: %+v", r)
	}
	visits, err := s.ListPipelineVisits(ctx, master.SwarmID, "race-run")
	if err != nil || len(visits) != 1 || visits[0].State != "inflight" {
		t.Fatalf("visit changed: %+v %v", visits, err)
	}
}

func TestGuestPipelineVisitCompletion(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	ctx := context.Background()
	s := ts.srv.store
	if err := s.CreatePipelineRun(ctx, store.PipelineRun{ID: "visit-run", SwarmID: master.SwarmID, OwnerID: master.DelegatorID, Graph: []byte(actorVisitGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	visit, err := s.AppendPipelineVisit(ctx, master.SwarmID, "visit-run", store.PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: worker.UID, ActorIncarnation: worker.Incarnation, RequestID: "req"})
	if err != nil {
		t.Fatal(err)
	}
	sw, err := s.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSwarm(ctx, store.Swarm{ID: "foreign", ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: "foreign", AtespaceName: "foreign", AtespaceUID: strPtr("foreign-namespace"), State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePipelineRun(ctx, store.PipelineRun{ID: "foreign-run", SwarmID: "foreign", OwnerID: master.DelegatorID, Graph: []byte(actorVisitGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "foreign", "foreign-run", store.PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: worker.UID, ActorIncarnation: worker.Incarnation, RequestID: "req"}); err != nil {
		t.Fatal(err)
	}
	call := func(uid string, generation int64, fields map[string]any) relay.SwarmResponse {
		t.Helper()
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return ts.srv.managed.handleSwarmRequest(ctx, uid, generation, relay.SwarmRequest{Tool: "pipeline_visit_complete", Arguments: data})
	}
	base := func() map[string]any {
		return map[string]any{"run_id": "visit-run", "sequence": visit.Sequence, "request_id": "req", "outcome": "success"}
	}
	forged := base()
	forged["actor_uid"] = worker.UID
	forged["incarnation"] = worker.Incarnation
	wrongRun := base()
	wrongRun["run_id"] = "unrelated"
	foreign := base()
	foreign["run_id"] = "foreign-run"
	wrongReq := base()
	wrongReq["request_id"] = "other"
	for _, tc := range []struct {
		name, uid string
		gen       int64
		args      map[string]any
	}{
		{"forged", master.UID, master.Incarnation, forged},
		{"other actor", master.UID, master.Incarnation, base()},
		{"stale", worker.UID, worker.Incarnation + 1, base()},
		{"unrelated run", worker.UID, worker.Incarnation, wrongRun},
		{"cross swarm", worker.UID, worker.Incarnation, foreign},
		{"wrong request", worker.UID, worker.Incarnation, wrongReq},
		{"empty", worker.UID, worker.Incarnation, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := call(tc.uid, tc.gen, tc.args); r.OK {
				t.Fatalf("unexpected success: %+v", r)
			}
		})
	}
	for range 2 {
		if r := call(worker.UID, worker.Incarnation, base()); !r.OK {
			t.Fatalf("completion/retry refused: %+v", r)
		}
	}
	conflict := base()
	conflict["outcome"] = "fail"
	if r := call(worker.UID, worker.Incarnation, conflict); r.OK {
		t.Fatalf("conflicting replay accepted: %+v", r)
	}
	visits, err := s.ListPipelineVisits(ctx, master.SwarmID, "visit-run")
	if err != nil || len(visits) != 2 {
		t.Fatalf("visits: %+v %v", visits, err)
	}
	if visits[0].Sequence != visit.Sequence || visits[0].NodeID != "actor" || visits[0].State != "completed" || visits[0].Outcome == nil || *visits[0].Outcome != "success" || visits[0].ActorUID != worker.UID || visits[0].ActorIncarnation != worker.Incarnation || visits[0].RequestID != "req" {
		t.Fatalf("completed actor visit: %+v", visits[0])
	}
	if visits[1].Sequence != visit.Sequence+1 || visits[1].NodeID != "done" || visits[1].State != "completed" {
		t.Fatalf("exit successor: %+v", visits[1])
	}
	run, err := s.GetPipelineRun(ctx, master.SwarmID, "visit-run")
	if err != nil || run.Status != "succeeded" {
		t.Fatalf("run: %+v %v", run, err)
	}
}
