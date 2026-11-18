package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Сервер регистрирует только attested ephemeral child и открывает его историю (#292).
// SPEC: Точный владелец/parent/source, без доставки; пустая роль не открывает
// обычные ad-hoc задачи или чужой payload, удалённый inventory не теряет отчёт.
// IMPLEMENTATION: Callback владельца в FULL-регистрации и scoped history lookup.
// RESULT: IMPLEMENTED
// STATUS: DONE
func ephemeralChildServerFixture(t *testing.T) (*dispatchServer, store.EphemeralChildIntent, store.SwarmChildTask) {
	t.Helper()
	d, intent, _ := ephemeralAttestationServerFixture(t)
	ctx := context.Background()
	if _, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "register-proof"); err != nil {
		t.Fatal(err)
	}
	child, err := d.srv.registerEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, intent, child
}

func TestEphemeralChildServerRegistrationUsesProofAndNeverDispatches(t *testing.T) {
	d, intent, child := ephemeralChildServerFixture(t)
	ctx := context.Background()
	before, messages := d.calls()
	retry, err := d.srv.registerEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
	if err != nil || retry != child || child.State != "pending" || child.Role != "" || child.Delivery != "" {
		t.Fatalf("registration changed identity or execution state: %+v %v", retry, err)
	}
	queued, err := d.srv.store.QueuedSwarmChildren(ctx)
	if err != nil || len(queued) != 0 {
		t.Fatalf("ephemeral child entered role dispatcher: %+v %v", queued, err)
	}
	proof, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := d.srv.store.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	text, err := ephemeralChildInstruction(ephemeralInstructionSource{Intent: intent, Allocation: allocation, Proof: proof, Actor: actor, Work: store.SwarmChildWork{Parent: intent.Parent, Task: child}})
	if err != nil || text == "" {
		t.Fatalf("real registered facts do not render: %v", err)
	}
	after, sent := d.calls()
	if len(after) != len(before) || len(sent) != len(messages) {
		t.Fatal("registration reattested or delivered work")
	}
}

func TestEphemeralChildServerRegistrationRefusesUntrustedScope(t *testing.T) {
	for _, scenario := range []string{"missing-proof", "unknown-proof", "owner", "parent", "viewer"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, _ := ephemeralAttestationServerFixture(t)
			ctx := context.Background()
			if scenario == "unknown-proof" {
				d.reply = func(guestAttest) (relay.SessionResponse, error) {
					return relay.SessionResponse{Outcome: "unknown"}, nil
				}
			}
			if scenario != "missing-proof" {
				_, _ = d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "register-proof")
			}
			user, parent := d.master.DelegatorID, intent.Parent
			switch scenario {
			case "owner":
				if err := d.srv.store.SetUserDisabled(ctx, user, true); err != nil {
					t.Fatal(err)
				}
			case "parent":
				parent.Sequence++
			case "viewer":
				viewer, err := d.srv.store.GetUserByUsername(ctx, "bob")
				if err != nil {
					t.Fatal(err)
				}
				user = viewer.ID
			}
			before, messages := d.calls()
			got, err := d.srv.registerEphemeralChild(ctx, user, parent, intent.ID)
			if err == nil || got != (store.SwarmChildTask{}) {
				t.Fatalf("invalid registration accepted: %+v %v", got, err)
			}
			raw, err := sql.Open("sqlite", d.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			err = raw.QueryRow(`SELECT count(*) FROM swarm_child_tasks`).Scan(&count)
			raw.Close()
			if err != nil || count != 0 {
				t.Fatalf("refusal exposed child: %d %v", count, err)
			}
			after, sent := d.calls()
			if len(before) != len(after) || len(messages) != len(sent) {
				t.Fatal("refusal activated guest")
			}
		})
	}
}

func TestEphemeralChildGuestReadsExactPayloadAndRetainedReportThroughRealRelay(t *testing.T) {
	d, intent, child := ephemeralChildServerFixture(t)
	ctx := context.Background()
	peer := d.worker
	actor, err := d.srv.store.GetManagedActor(ctx, child.Executor.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	d.worker = actor
	parent, executor, messages := childRelay(t, d)
	for _, caller := range []relay.SwarmCaller{parent, executor} {
		response := childCall(t, caller, "child_get", map[string]any{"child_id": child.ID})
		var view childTaskJSON
		if !response.OK || json.Unmarshal(response.Data, &view) != nil || view.ID != child.ID || view.Task != intent.Payload || view.Role != "" || view.State != "pending" {
			t.Fatalf("exact child history unavailable: %+v", response)
		}
	}
	if _, err := d.srv.managed.childStatus(ctx, peer, child.ID); err == nil {
		t.Fatal("peer read foreign ephemeral payload")
	}
	legacy, err := d.srv.store.ReserveSwarmChild(ctx, intent.Parent, "legacy", "Not an ephemeral child", childPrincipal(peer, intent.Parent.Principal.ProjectID), d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.managed.childStatus(ctx, d.master, legacy.ID); err == nil {
		t.Fatal("empty role exposed ordinary ad-hoc child")
	}
	allocation, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	text, err := ephemeralChildInstruction(ephemeralInstructionSource{Intent: intent, Allocation: allocation, Proof: proof, Actor: actor, Work: store.SwarmChildWork{Parent: intent.Parent, Task: child}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(relay.MessageRequest{To: actor.Name, From: stepMessageSender(d.swarm), Body: text})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := d.srv.store.PrepareEphemeralChildDelivery(ctx, intent.ID, store.ActorDeliveryInput{SwarmID: d.swarm, SourceKind: "child", SourceID: child.ID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: actor.UID, Incarnation: actor.Incarnation, AtespaceUID: actor.AtespaceUID, JournalID: strings.Repeat("c", 64), Payload: string(payload)}, d.srv.managed.childAuthority)
	if err != nil || delivery.Input.SourceKind != "child" || delivery.Input.SourceID != child.ID || delivery.Input.Payload != string(payload) {
		t.Fatalf("instruction and child start were not reserved together: %+v %v", delivery, err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || work.Task.State != "running" || work.Task.Revision != child.Revision+1 {
		t.Fatalf("preparation did not start exact child: %+v %v", work, err)
	}
	child = work.Task
	report := "Independent result retained outside the ephemeral workspace."
	response := childCall(t, executor, "child_complete", map[string]any{"child_id": child.ID, "request_id": child.RequestID, "outcome": "success", "report": report})
	if !response.OK {
		t.Fatalf("exact executor report refused: %+v", response)
	}
	raw, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Проверяется только чтение истории после отсутствия inventory, не физический cleanup.
	_, err = raw.Exec(`DELETE FROM managed_actors WHERE uid=?`, actor.UID)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	response = childCall(t, parent, "child_get", map[string]any{"child_id": child.ID})
	var view childTaskJSON
	if !response.OK || json.Unmarshal(response.Data, &view) != nil || view.State != "completed" || view.Outcome != "success" || view.Report != report || view.Task != intent.Payload {
		t.Fatalf("history depended on live inventory: %+v", response)
	}
	if messages.Load() != 0 {
		t.Fatal("registration or read sent an instruction")
	}
}
