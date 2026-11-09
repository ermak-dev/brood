package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// FEATURE: Reserve native actor creation before an actor UID exists.
// SPEC: Only one current visit is reserved; retries never authorize resending,
// identities are immutable, and recovery leaves creation uncertain.
// IMPLEMENTATION: Persist native creation identity before binding the actor UID.
// RESULT: IMPLEMENTED — replays and recovery never authorize another creation.
// STATUS: DONE
// BUG: Late timeouts and bind replays can invalidate or accept stale visits.
// HYPOTHESIS: The timeout update ignores bindings, and replay ignores visit state and sequence.
// SOLUTION: Guard timeout updates and require an inflight current visit for bind replay.
// RESULT: FIXED — bound visits survive late timeouts and stale replays fail closed.
// STATUS: FIXED
func TestPipelineNativeReservationRaces(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"late timeout", "unconfirmed", "terminal", "superseded", "recovery bound", "recovery unbound"} {
		t.Run(scenario, func(t *testing.T) {
			s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'),('bind','p','u','bind','bind','now','now')`)
			if err != nil {
				t.Fatal(err)
			}
			if err = attachTestGraph(ctx, s, "s", actorStartGraph); err != nil {
				t.Fatal(err)
			}
			r := PipelineNativeReservation{SwarmID: "s", RunID: "s", Sequence: 1, NodeID: "actor", RequestID: "req", TemplateName: "development", TemplateUID: "template-uid", TemplateDigest: "sha256:" + strings.Repeat("a", 64)}
			if _, err = s.ReservePipelineNativeActor(ctx, r); err != nil {
				t.Fatal(err)
			}
			if scenario != "recovery unbound" {
				if err = s.BindPipelineNativeActor(ctx, r, "uid", 1); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "late timeout":
				if err = s.MarkPipelineNativeActorUncertain(ctx, r); err == nil {
					t.Error("late timeout accepted after bind")
				}
				var state string
				if err = s.db.QueryRow(`SELECT state FROM swarm_steps WHERE swarm_id='s' AND sequence=1`).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != "inflight" {
					t.Errorf("late timeout changed bound state to %s", state)
				}
				return
			case "unconfirmed":
				_, err = s.db.Exec(`UPDATE swarm_steps SET state='unconfirmed' WHERE swarm_id='s'`)
			case "terminal":
				_, err = s.db.Exec(`UPDATE swarm_steps SET state='completed' WHERE swarm_id='s'`)
			case "superseded":
				_, err = s.db.Exec(`DROP TRIGGER pipeline_preparations_step_no_replace; INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('s',2,'actor','pending')`)
			case "recovery bound", "recovery unbound":
				err = s.RecoverPipelineActions(ctx)
				if err == nil {
					got, e := s.ReservePipelineNativeActor(ctx, r)
					if e != nil || !got.Replay || got.State != "unconfirmed" {
						t.Errorf("recovery replay: %+v %v", got, e)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.BindPipelineNativeActor(ctx, r, "uid", 1); err == nil {
				t.Error("bind accepted stale or unconfirmed visit")
			}
		})
	}
}

// BUG: A styled native actor cannot reserve its effective template.
// HYPOTHESIS: Reservation inspects only the explicit template field.
// SOLUTION: Resolve the effective template and reject managed pool reservations.
// RESULT: FIXED — styled native reservations bind the effective template.
// STATUS: FIXED
func TestPipelineStyledNativeReservation(t *testing.T) {
	ctx := context.Background()
	s := sourceTestStore(t)
	graph := strings.Replace(actorStartGraph, "template: development, ", "", 1) + "styles:\n  - selectors: [actor]\n    template: development\n"
	if err := attachTestGraph(ctx, s, "s", graph); err != nil {
		t.Fatalf("attach styled graph: %v", err)
	}
	r := PipelineNativeReservation{SwarmID: "s", RunID: "s", Sequence: 1, NodeID: "actor", RequestID: "req", TemplateName: "wrong", TemplateUID: "template-uid", TemplateDigest: "sha256:" + strings.Repeat("a", 64)}
	if _, err := s.ReservePipelineNativeActor(ctx, r); err == nil {
		t.Fatal("accepted wrong styled template")
	}
	r.TemplateName = "development"
	if _, err := s.ReservePipelineNativeActor(ctx, r); err != nil {
		t.Fatalf("reserve effective styled template: %v", err)
	}
}

func TestPipelineNativeReservation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at,graph,execution,definition_name,definition_version,definition_revision) VALUES ('s','p','u','swarm','space','now','now',NULL,'idle',NULL,NULL,NULL); INSERT INTO swarm_members VALUES ('s','u','owner')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = attachTestGraph(ctx, s, "s", actorStartGraph); err != nil {
		t.Fatal(err)
	}
	req := PipelineNativeReservation{SwarmID: "s", RunID: "s", Sequence: 1, NodeID: "actor", RequestID: "req", TemplateName: "development", TemplateUID: "template-uid", TemplateDigest: "sha256:" + strings.Repeat("a", 64)}
	var wg sync.WaitGroup
	results := make(chan PipelineNativeReservation, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ReservePipelineNativeActor(ctx, req)
			results <- got
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	fresh := 0
	var name string
	for got := range results {
		if !got.Replay {
			fresh++
		}
		if got.ActorName == "" {
			t.Fatal("empty name")
		}
		if name != "" && got.ActorName != name {
			t.Fatal("name changed")
		}
		name = got.ActorName
	}
	if fresh != 1 {
		t.Fatalf("fresh reservations: %d", fresh)
	}
	bad := req
	bad.TemplateUID = "other"
	if _, err = s.ReservePipelineNativeActor(ctx, bad); err == nil {
		t.Fatal("accepted different template")
	}
	bad = req
	bad.TemplateDigest = ""
	if _, err = s.ReservePipelineNativeActor(ctx, bad); err == nil {
		t.Fatal("accepted missing digest")
	}
	bad = req
	bad.RequestID = "other"
	if _, err = s.ReservePipelineNativeActor(ctx, bad); err == nil {
		t.Fatal("accepted second request")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RecoverPipelineActions(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReservePipelineNativeActor(ctx, req)
	if err != nil || !got.Replay || got.State != "unconfirmed" {
		t.Fatalf("recovery replay: %+v %v", got, err)
	}
	if err = s.BindPipelineNativeActor(ctx, req, "uid", 1); err == nil {
		t.Fatal("bound uncertain creation")
	}
	if _, err = s.db.Exec(`INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('bind','p','u','bind','bind','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err = attachTestGraph(ctx, s, "bind", actorStartGraph); err != nil {
		t.Fatal(err)
	}
	req.SwarmID, req.RunID = "bind", "bind"
	if _, err = s.ReservePipelineNativeActor(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err = s.BindPipelineNativeActor(ctx, req, "uid", 1); err != nil {
		t.Fatal(err)
	}
	if err = s.BindPipelineNativeActor(ctx, req, "uid", 1); err != nil {
		t.Fatal(err)
	}
	if err = s.BindPipelineNativeActor(ctx, req, "other", 1); err == nil {
		t.Fatal("rebound actor uid")
	}
	if err = s.BindPipelineNativeActor(ctx, req, "uid", 2); err == nil {
		t.Fatal("rebound incarnation")
	}
	if err = s.CompletePipelineActorVisit(ctx, "bind", "bind", 1, "req", "uid", 1, "success"); err != nil {
		t.Fatal(err)
	}
	got, err = s.ReservePipelineNativeActor(ctx, req)
	if err != nil || !got.Replay || got.State != "completed" {
		t.Fatalf("completed replay: %+v %v", got, err)
	}
}
