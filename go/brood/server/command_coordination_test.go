package server

// FEATURE: Managed master dispatch leases and project-wide advisory landing tokens.
// SPEC: Recheck delegation and namespace; dispatch once per durable claim, retain
// uncertain delivery claims, never resurrect released requests, and release only
// the recorded landing holder. A lease is not workspace cleanliness proof.
// IMPLEMENTATION: UID-bound authorization, transactional claim acquisition and
// non-resending retries, explicit delivery state, and exact-holder token release.
// RESULT: IMPLEMENTED — full Go suite, coordination race tests and vet pass.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func coordinationInvocation(name string, args map[string]any) command.Invocation {
	data, _ := json.Marshal(args)
	return command.Invocation{Name: name, Arguments: data}
}

// coordinationServerSetup заводит master и актора "worker", созданного
// оператором. Назначить ему работу master не может (#267): проверки dispatch
// берут worker, добавленного master, из ownedWorkerSetup.
func coordinationServerSetup(t *testing.T, targetRole ...string) (*substrateServer, store.ManagedActor, store.ManagedActor) {
	t.Helper()
	ts, alice, master, pve := masterSetup(t)
	pve.running = false
	pve.nextIDs = []int{106}
	input := map[string]any{"metadata": map[string]string{"name": "worker"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}}
	if len(targetRole) != 0 {
		input["role"] = targetRole[0]
	}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+master.SwarmID+"/actors", ts.csrf(alice), input)
	expectStatus(t, "worker", r, b, 201)
	// The provider fake has one container state: reconcile only the worker so its
	// ownership metadata does not replace the already-ready master's observation.
	for range 4 {
		worker, err := ts.srv.store.LookupManagedActor(context.Background(), master.SwarmID, "worker")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.srv.managed.reconcileActor(context.Background(), worker); err != nil {
			t.Fatal(err)
		}
	}
	worker, err := ts.srv.store.LookupManagedActor(context.Background(), master.SwarmID, "worker")
	if err != nil || worker.Phase != "running" {
		t.Fatalf("worker: %+v %v", worker, err)
	}
	return ts, master, worker
}

func coordinationData(t *testing.T, result command.Result) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("result data: %v (%+v)", err, result)
	}
	return data
}

func TestCoordinationDispatchOnceAndReleasedRetry(t *testing.T) {
	ts, master, worker := ownedWorkerSetup(t)
	ctx := context.Background()
	var calls atomic.Int32
	ts.srv.managed.deliverMessage = func(_ context.Context, uid string, generation int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		calls.Add(1)
		if uid != worker.UID || generation != worker.Incarnation || req.From != master.Name || req.To != worker.Name || req.Body != "task" {
			t.Errorf("wrong delivery: %s %d %+v", uid, generation, req)
		}
		return relay.MessageResponse{OK: true}, nil
	}
	inv := coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "once"})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv); err != nil {
				t.Errorf("dispatch: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("deliveries=%d", calls.Load())
	}
	sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	claim, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, master.SwarmID, worker.UID)
	if err != nil || claim.RequestID != "once" || claim.Target.Generation != worker.Incarnation || claim.HumanID != master.DelegatorID {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if err := ts.srv.store.ReleaseDispatchLease(ctx, claim); err != nil {
		t.Fatal(err)
	}
	got, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("released retry: %+v %v deliveries=%d", got, err, calls.Load())
	}
	for _, detail := range []string{"once", worker.Name, worker.UID, "not_retried", "lease_active=false"} {
		if !strings.Contains(got.Text, detail) {
			t.Fatalf("text-only adapter loses %q: %s", detail, got.Text)
		}
	}
	data := coordinationData(t, got)
	if data["request_id"] != "once" || data["target_uid"] != worker.UID || data["delivery"] != "not_retried" || data["lease_active"] != false {
		t.Fatalf("retry: %s", got.Data)
	}
	if _, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, master.SwarmID, worker.UID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resurrected: %v", err)
	}
}

func TestCoordinationDispatchUnconfirmedAndOccupied(t *testing.T) {
	for _, negativeAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "negative_ack"}[negativeAck], func(t *testing.T) {
			ts, master, worker := ownedWorkerSetup(t)
			ctx := context.Background()
			calls := 0
			ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
				calls++
				if negativeAck {
					return relay.MessageResponse{OK: false}, nil
				}
				return relay.MessageResponse{}, errors.New("private transport detail")
			}
			inv := coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "uncertain"})
			got, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv)
			if err != nil || coordinationData(t, got)["delivery"] != "unconfirmed" || coordinationData(t, got)["lease_active"] != true {
				t.Fatalf("uncertain: %+v %v", got, err)
			}
			sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
			if _, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, master.SwarmID, worker.UID); err != nil {
				t.Fatal(err)
			}
			if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv); err != nil {
				t.Fatal(err)
			}
			if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "overwrite", "request_id": "other"})); err == nil {
				t.Fatal("occupied lease accepted")
			}
			if calls != 1 {
				t.Fatalf("deliveries=%d", calls)
			}
		})
	}
}

func TestCoordinationAuthorizationAndInput(t *testing.T) {
	for _, scenario := range []string{"stale", "revoked", "forged_scope", "foreign_target", "self", "empty_request", "empty_message", "namespace", "target_stale", "workspace_proof", "landing_name", "landing_request"} {
		t.Run(scenario, func(t *testing.T) {
			ts, master, worker := ownedWorkerSetup(t)
			ctx := context.Background()
			args := map[string]any{"to": "worker", "message": "task", "request_id": "denied"}
			name := "dispatch"
			switch scenario {
			case "stale":
				master.Incarnation++
			case "revoked":
				if err := ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, false); err != nil {
					t.Fatal(err)
				}
			case "forged_scope":
				master.SwarmID = "foreign"
			case "foreign_target":
				args["to"] = "foreign"
			case "self":
				args["to"] = master.Name
			case "empty_request":
				delete(args, "request_id")
			case "empty_message":
				args["message"] = ""
			case "namespace":
				sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
				ts.fake.mu.Lock()
				ts.fake.atespaces[sw.AtespaceName].Metadata.Uid = "replacement"
				ts.fake.mu.Unlock()
			case "target_stale":
				worker.Phase = "deleting"
				if _, err := ts.srv.store.UpdateManagedActor(ctx, worker, worker.Revision); err != nil {
					t.Fatal(err)
				}
			case "workspace_proof":
				args["require_clean_workspace"] = true
			case "landing_name":
				name = "landing"
				args = map[string]any{"action": "take", "name": "worker", "request_id": "denied"}
			case "landing_request":
				name = "landing"
				args = map[string]any{"action": "take"}
			}
			calls := 0
			ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
				calls++
				return relay.MessageResponse{OK: true}, nil
			}
			if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation(name, args)); err == nil {
				t.Fatal("boundary accepted")
			}
			if calls != 0 {
				t.Fatal("unauthorized delivery")
			}
		})
	}
}

func TestCoordinationLandingHolderAndConflict(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t, "master")
	ctx := context.Background()
	run := func(from store.ManagedActor, args map[string]any) (command.Result, error) {
		return ts.srv.managed.executeCoordinationCommand(ctx, from, coordinationInvocation("landing", args))
	}
	got, err := run(master, map[string]any{"action": "take", "request_id": "landing-one"})
	if err != nil || coordinationData(t, got)["request_id"] != "landing-one" {
		t.Fatalf("take: %+v %v", got, err)
	}
	if !strings.Contains(got.Text, "landing-one") || !strings.Contains(got.Text, master.UID) {
		t.Fatalf("text-only adapter loses landing holder: %s", got.Text)
	}
	if _, err := run(master, map[string]any{"action": "take", "request_id": "landing-two"}); err == nil {
		t.Fatal("second request stole token")
	}
	if _, err := run(worker, map[string]any{"action": "give"}); err == nil {
		t.Fatal("wrong holder released")
	}
	if _, err := run(master, map[string]any{"action": "give", "request_id": "wrong"}); err == nil {
		t.Fatal("wrong request released")
	}
	if _, err := run(master, map[string]any{"action": "give"}); err != nil {
		t.Fatal(err)
	}
	sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if _, err := ts.srv.store.ShowLandingToken(ctx, sw.ProjectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("not released: %v", err)
	}
	got, err = run(master, map[string]any{"action": "take", "request_id": "landing-one"})
	if err != nil {
		t.Fatal(err)
	}
	if data := coordinationData(t, got); data["request_id"] != "landing-one" || data["holder_uid"] != master.UID || data["active"] != false {
		t.Fatalf("released take must report its inactive original claim: %s", got.Data)
	}
	if _, err := ts.srv.store.ShowLandingToken(ctx, sw.ProjectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("retry resurrected token: %v", err)
	}
	if _, err := run(worker, map[string]any{"action": "take", "request_id": "successor"}); err != nil {
		t.Fatal(err)
	}
	got, err = run(master, map[string]any{"action": "take", "request_id": "landing-one"})
	if err != nil {
		t.Fatal(err)
	}
	if data := coordinationData(t, got); data["request_id"] != "landing-one" || data["holder_uid"] != master.UID || data["active"] != false {
		t.Fatalf("released take reported successor ownership: %s", got.Data)
	}
	if holder, err := ts.srv.store.ShowLandingToken(ctx, sw.ProjectID); err != nil || holder.Source.UID != worker.UID {
		t.Fatalf("retry changed successor: %+v %v", holder, err)
	}
	if err := ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := run(master, map[string]any{"action": "show"}); err == nil {
		t.Fatal("revoked show accepted")
	}
}

func TestCoordinationRechecksAfterDelivery(t *testing.T) {
	ts, master, _ := ownedWorkerSetup(t)
	ts.srv.managed.deliverMessage = func(ctx context.Context, _ string, _ int64, _ relay.MessageRequest) (relay.MessageResponse, error) {
		if err := ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true); err != nil {
			t.Fatal(err)
		}
		return relay.MessageResponse{OK: true}, nil
	}
	got, err := ts.srv.managed.executeCoordinationCommand(context.Background(), master, coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "revoke"}))
	if err == nil || len(got.Data) != 0 {
		t.Fatalf("private result returned after revocation: %+v %v", got, err)
	}
}
