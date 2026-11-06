package store

import (
	"context"
	"strings"
	"testing"
)

// FEATURE: Pipeline runs name their swarm, project, owner and stage actors (#191).
// SPEC: Run reads carry the swarm name, project and owner username of the run's
// own swarm. A visit names its actor by the reserved native name, else by the
// managed actor with its UID in the run's swarm, including deleted actors.
// A managed UID of another swarm and an unknown UID stay unnamed. The swarm-wide
// visit read returns only runs of that swarm, keyed by run.
// IMPLEMENTATION: Join the swarm, project and owner rows into run reads and
// resolve managed names with a swarm-scoped outer join.
// RESULT: IMPLEMENTED — runs and visits carry display context without extra queries.
// STATUS: DONE
const stageGraph = `apiVersion: brood.dev/v1alpha1
name: stages
start: a
exit: done
nodes:
  a: {type: actor, template: development, prompt: work}
  b: {type: actor, template: development, prompt: work}
  c: {type: actor, template: development, prompt: work}
  d: {type: actor, template: development, prompt: work}
  e: {type: actor, template: development, prompt: work}
  done: {type: exit}
edges:
  - {from: a, to: b}
  - {from: b, to: c}
  - {from: c, to: d}
  - {from: d, to: e}
  - {from: e, to: done}
goalGates: [done]
`

func TestPipelineRunOrigin(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	// Различные ID и имена ловят перепутанные колонки.
	if _, err := s.db.Exec(`UPDATE projects SET name='Project One' WHERE id='one'; UPDATE projects SET name='Project Two' WHERE id='two'; UPDATE swarms SET name='alpha' WHERE id='one'; UPDATE swarms SET name='beta' WHERE id='two'`); err != nil {
		t.Fatal(err)
	}
	seedUser(t, s, "runner", "run-owner")
	gone := managedRequest("uid-gone", "one", 101)
	gone.Name = "gone"
	if _, err := s.ReserveManagedActor(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE managed_actors SET phase='deleted' WHERE uid='uid-gone'`); err != nil {
		t.Fatal(err)
	}
	for _, a := range []ManagedActor{managedRequest("uid-worker", "one", 102), managedRequest("uid-foreign", "two", 103)} {
		a.Name = strings.TrimPrefix(a.UID, "uid-")
		if _, err := s.ReserveManagedActor(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []PipelineRun{
		{ID: "r1", SwarmID: "one", OwnerID: "runner", Graph: []byte(stageGraph), Status: "running"},
		{ID: "r2", SwarmID: "two", OwnerID: "u", Graph: []byte(stageGraph), Status: "running"},
		{ID: "r3", SwarmID: "one", OwnerID: "u", Graph: []byte(stageGraph), Status: "running"},
	} {
		if err := s.CreatePipelineRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for i, uid := range []string{"uid-worker", "uid-foreign", "uid-gone", "uid-unknown"} {
		node, request := string(rune('a'+i)), "req-"+uid
		v, err := s.AppendPipelineVisit(ctx, "one", "r1", PipelineVisit{NodeID: node, State: "inflight", ActorUID: uid, ActorIncarnation: 1, RequestID: request})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CompletePipelineActorVisit(ctx, "one", "r1", v.Sequence, request, uid, 1, "success"); err != nil {
			t.Fatal(err)
		}
	}
	native, err := s.ReservePipelineNativeActor(ctx, PipelineNativeReservation{SwarmID: "one", RunID: "r1", NodeID: "e", RequestID: "req-native", Sequence: 5, TemplateName: "development", TemplateUID: "template-uid", TemplateDigest: "sha256:" + strings.Repeat("ab", 32)})
	if err != nil || native.ActorName == "" {
		t.Fatalf("native reservation: %+v %v", native, err)
	}
	// Совпадение UID с управляемым актором не перебивает имя нативной резервации.
	if err := s.BindPipelineNativeActor(ctx, native, "uid-worker", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "two", "r2", PipelineVisit{NodeID: "a", State: "inflight", ActorUID: "uid-worker", ActorIncarnation: 1, RequestID: "req"}); err != nil {
		t.Fatal(err)
	}

	one := PipelineRunOrigin{SwarmName: "alpha", ProjectID: "one", ProjectName: "Project One"}
	run, err := s.GetPipelineRun(ctx, "one", "r1")
	if want := withOwner(one, "run-owner"); err != nil || run.Origin != want {
		t.Fatalf("run origin: %+v %v, want %+v", run.Origin, err, want)
	}
	runs, err := s.ListPipelineRuns(ctx, "one")
	if err != nil || len(runs) != 2 || runs[0].ID != "r1" || runs[0].Origin != withOwner(one, "run-owner") || runs[1].ID != "r3" || runs[1].Origin != withOwner(one, "managed-user") {
		t.Fatalf("listed origins: %+v %v", runs, err)
	}
	runs, err = s.ListPipelineRuns(ctx, "two")
	if want := (PipelineRunOrigin{SwarmName: "beta", ProjectID: "two", ProjectName: "Project Two", OwnerUsername: "managed-user"}); err != nil || len(runs) != 1 || runs[0].ID != "r2" || runs[0].Origin != want {
		t.Fatalf("foreign origins: %+v %v", runs, err)
	}

	names := func(visits []PipelineVisit) []string {
		out := []string{}
		for _, v := range visits {
			out = append(out, v.NodeID+"="+v.ActorName)
		}
		return out
	}
	want := "a=worker b= c=gone d= e=" + native.ActorName
	visits, err := s.ListPipelineVisits(ctx, "one", "r1")
	if got := strings.Join(names(visits), " "); err != nil || got != want {
		t.Fatalf("visit actors: %q %v, want %q", got, err, want)
	}
	visits, err = s.ListPipelineVisits(ctx, "two", "r2")
	if got := strings.Join(names(visits), " "); err != nil || got != "a=" {
		t.Fatalf("cross-swarm managed UID must stay unnamed: %q %v", got, err)
	}
	byRun, err := s.ListSwarmPipelineVisits(ctx, "one")
	if err != nil || len(byRun) != 1 || strings.Join(names(byRun["r1"]), " ") != want {
		t.Fatalf("swarm visits: %+v %v", byRun, err)
	}
	byRun, err = s.ListSwarmPipelineVisits(ctx, "two")
	if err != nil || len(byRun) != 1 || strings.Join(names(byRun["r2"]), " ") != "a=" || byRun["r2"][0].ActorUID != "uid-worker" {
		t.Fatalf("foreign swarm visits: %+v %v", byRun, err)
	}
}

func withOwner(o PipelineRunOrigin, username string) PipelineRunOrigin {
	o.OwnerUsername = username
	return o
}
