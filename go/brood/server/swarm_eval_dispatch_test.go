package server

// FEATURE: Автоматический replay сохранённого ответа (#293).
// SPEC: Restart scan находит waiting eval, provenance сохраняет reference
// и RecordedBy, mismatch останавливается долговечно и не повторяется.
// IMPLEMENTATION: Bounded pending scan и replay до обычных эффектов графа;
// FULL callback проверяет authority и неизменность inventory под effect gates.
// RESULT: IMPLEMENTED — provenance, revocation, gate race и restart-stop проходят.
// STATUS: DONE

import (
	"context"
	"errors"
	"time"

	"brood.dev/go/brood/store"
	"net/http"
	"strings"
	"testing"
)

func TestEvalDispatchRestartReplayExactProvenance(t *testing.T) {
	f := evalAPISetup(t)
	ctx := context.Background()
	s := f.ts.srv.store
	f.post(t, "/eval", f.input, http.StatusOK)
	// Отключаем асинхронное пробуждение: следующий scan имитирует рестарт.
	f.ts.srv.steps.close()
	f.post(t, "/eval/start", nil, http.StatusOK)
	requests, err := s.SwarmRequests(ctx, f.candidate)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests %v %v", requests, err)
	}
	d := newStepDispatcher(f.ts.srv)
	d.start(ctx)
	defer d.close()
	d.wait()
	decision, err := s.GetSwarmEvalReplay(ctx, f.candidate, requests[0].ID)
	ref, _ := s.SwarmRequests(ctx, f.reference)
	if err != nil || decision.State != "applied" || decision.Answer == nil || decision.Answer.RecordedBy != f.recorder || decision.Answer.ReferenceRequestID != ref[0].ID || decision.Answer.Response == nil || *decision.Answer.Response != "Retained original notes" {
		t.Fatalf("replay lost provenance: %+v %v", decision, err)
	}
	recorded, err := s.GetSwarmRequest(ctx, f.candidate, requests[0].ID)
	if err != nil || recorded.AnsweredBy != f.launcher || f.recorder == f.launcher {
		t.Fatalf("reference human impersonated: %+v %v", recorded, err)
	}
	sw, _ := s.GetSwarm(ctx, f.candidate)
	if sw.Execution != "succeeded" {
		t.Fatalf("execution %s", sw.Execution)
	}
}
func TestEvalDispatchRechecksDurableLauncherAuthority(t *testing.T) {
	for _, scope := range []string{"candidate", "reference", "disabled"} {
		t.Run(scope, func(t *testing.T) {
			f := evalAPISetup(t)
			ctx := context.Background()
			s := f.ts.srv.store
			f.ts.srv.steps.close()
			f.post(t, "/eval", f.input, http.StatusOK)
			f.post(t, "/eval/start", nil, http.StatusOK)
			requests, _ := s.SwarmRequests(ctx, f.candidate)
			var err error
			if scope == "disabled" {
				err = s.SetUserDisabled(ctx, f.launcher, true)
			} else {
				id := f.candidate
				if scope == "reference" {
					id = f.reference
				}
				sw, _ := s.GetSwarm(ctx, id)
				err = s.RemoveMember(ctx, sw.ProjectID, f.launcher)
			}
			if err != nil {
				t.Fatal(err)
			}
			d := newStepDispatcher(f.ts.srv)
			d.start(ctx)
			d.wait()
			d.close()
			if _, err = s.GetSwarmEvalReplay(ctx, f.candidate, requests[0].ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revoked replay persisted: %v", err)
			}
			req, _ := s.GetSwarmRequest(ctx, f.candidate, requests[0].ID)
			sw, _ := s.GetSwarm(ctx, f.candidate)
			if req.State != "open" || req.AnsweredBy != "" || sw.Execution != "waiting" {
				t.Fatalf("revoked replay changed execution: %+v %+v", req, sw)
			}
		})
	}
}

func evalDispatchActor(t *testing.T, f evalAPIFixture, name string, ctid int) {
	t.Helper()
	ctx := context.Background()
	s := f.ts.srv.store
	if name == "first" {
		_, err := s.CreateManagedPool(ctx, store.ManagedPool{UID: "eval-pool", Name: "eval-pool", Connection: "configured", Source: store.ManagedSourceSnapshot{ClusterIdentity: "cluster", Endpoint: "https://pve.invalid:8006", Node: "node", ResourcePool: "brood", Storage: "local", Network: "vmbr0"}, MaxActors: 8, Resources: store.ManagedResources{CPUMillicores: 8000, MemoryBytes: 8192, DiskBytes: 16384}, Template: store.ManagedTemplateSnapshot{Profile: "claude-proxmox", URI: "local:template.tar.zst", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, State: "enabled"})
		if err != nil {
			t.Fatal(err)
		}
	}
	sw, err := s.GetSwarm(ctx, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReserveManagedActor(ctx, store.ManagedActor{UID: name, Name: name, SwarmID: f.candidate, AtespaceUID: *sw.AtespaceUID, PoolUID: "eval-pool", Resources: store.ManagedResources{CPUMillicores: 1000, MemoryBytes: 1024, DiskBytes: 2048}, CTID: ctid, OwnerNonce: "owner-" + name, OperationID: "create-" + name})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEvalDispatchEffectGatesAndInventoryRecheck(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "gated", true: "new-actor"}[change], func(t *testing.T) {
			f := evalAPISetup(t)
			ctx := context.Background()
			s := f.ts.srv.store
			f.ts.srv.steps.close()
			f.post(t, "/eval", f.input, http.StatusOK)
			f.post(t, "/eval/start", nil, http.StatusOK)
			requests, _ := s.SwarmRequests(ctx, f.candidate)
			evalDispatchActor(t, f, "first", 101)
			// Точный retry остаётся допустимым после появления eval inventory.
			f.post(t, "/eval", f.input, http.StatusOK)
			gates := &f.ts.srv.managed.effects
			release, err := gates.acquire(ctx, "first")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { release() }()
			d := newStepDispatcher(f.ts.srv)
			done := make(chan bool, 1)
			go func() { done <- d.replayEval(ctx, f.candidate) }()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			deadline := time.After(5 * time.Second)
			waiting := false
			for !waiting {
				gates.mu.Lock()
				entry := gates.entries["first"]
				waiting = entry != nil && entry.users == 2
				gates.mu.Unlock()
				if waiting {
					break
				}
				select {
				case <-ticker.C:
				case <-deadline:
					t.Fatal("replay never acquired effect gate")
				}
			}
			if _, err = s.GetSwarmEvalReplay(ctx, f.candidate, requests[0].ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("replay bypassed gate: %v", err)
			}
			if change {
				evalDispatchActor(t, f, "second", 102)
			}
			release()
			// Не освобождаем один и тот же gate повторно в defer.
			release = func() {}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("replay stuck after gate release")
			}
			decision, err := s.GetSwarmEvalReplay(ctx, f.candidate, requests[0].ID)
			if change {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("ungated actor allowed replay: %+v %v", decision, err)
				}
			} else if err != nil || decision.State != "applied" {
				t.Fatalf("gated replay: %+v %v", decision, err)
			}
		})
	}
}

func TestEvalDispatchMismatchStopSurvivesRestart(t *testing.T) {
	f := evalAPISetup(t)
	ctx := context.Background()
	s := f.ts.srv.store
	sw, _ := s.GetSwarm(ctx, f.candidate)
	root, _ := s.GetUserByUsername(ctx, "root")
	_, err := s.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"approve": strings.ReplaceAll(startAPIApprove, "label: Approve", "label: Different choice")}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	f.ts.srv.steps.close()
	f.post(t, "/eval", f.input, http.StatusOK)
	f.post(t, "/eval/start", nil, http.StatusOK)
	requests, _ := s.SwarmRequests(ctx, f.candidate)
	for i := 0; i < 2; i++ {
		d := newStepDispatcher(f.ts.srv)
		d.start(ctx)
		d.wait()
		d.close()
		sw, _ = s.GetSwarm(ctx, f.candidate)
		if sw.Execution != "stopped" {
			t.Fatalf("scan %d execution %s", i, sw.Execution)
		}
		decision, err := s.GetSwarmEvalReplay(ctx, f.candidate, requests[0].ID)
		if err != nil || decision.State != "stopped" || decision.Reason == "" || decision.Answer != nil {
			t.Fatalf("stop: %+v %v", decision, err)
		}
		r, status, _ := f.ts.do(f.alice, "GET", "/api/swarms/"+f.candidate+"/eval", "", nil)
		expectStatus(t, "retained stop status", r, status, http.StatusOK)
		replay, ok := status["replay"].(map[string]any)
		if !ok || replay["state"] != "stopped" || replay["reason"] != decision.Reason || replay["requestId"] != requests[0].ID {
			t.Fatalf("status lost durable refusal: %v", status)
		}
	}
}
