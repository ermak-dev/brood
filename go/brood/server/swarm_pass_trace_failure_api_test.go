package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Human trace показывает pre-registration отказ отдельно от результата (#292).
// SPEC: Исходная причина и время решения, без synthetic child/proof/report/deletion;
// резерв и физическая неопределённость остаются отдельными неизменными фактами.
// IMPLEMENTATION: Явный failure DTO только с исходными reason/failedAt.
// RESULT: IMPLEMENTED — owner/download/peer isolation, отсутствие эффектов и full/race зелёные.
// STATUS: DONE
func TestPassTraceAPIExportsUnregisteredFailureWithoutInventingResult(t *testing.T) {
	for _, allocated := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "reserved"}[allocated], func(t *testing.T) {
			ctx := context.Background()
			d, intent, reservation := ephemeralCapacityFixture(t)
			if allocated {
				if _, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation); err != nil {
					t.Fatal(err)
				}
				if _, err := d.srv.store.ReserveEphemeralChildCreate(ctx, intent.ID, d.srv.managed.childAuthority); err != nil {
					t.Fatal(err)
				}
			}
			reason := "Server rejected this job before bootstrap.\nNo execution outcome is claimed."
			if err := d.srv.store.RecordEphemeralChildFailure(ctx, intent.ID, reason, d.srv.managed.childAuthority); err != nil {
				t.Fatal(err)
			}
			retained, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, messages := d.calls()
			path := "/api/swarms/" + d.swarm + "/passes/1"
			response, body, _ := d.do(d.alice, "GET", path, "", nil)
			expectStatus(t, "unregistered failure history", response, body, http.StatusOK)
			// Незарегистрированный отказ занимает логический слот, но не создаёт ребёнка.
			group := body["steps"].([]any)[0].(map[string]any)["children"].(map[string]any)
			if group["unregisteredFailed"] != float64(1) || group["registered"] != float64(0) || group["total"] != float64(2) || group["open"] != float64(1) {
				t.Fatalf("failure accounting is not reconstructible from public child group: %+v", group)
			}
			response, history, _ := d.do(d.alice, "GET", "/api/swarms/"+d.swarm+"/children/1", "", nil)
			expectStatus(t, "failed job child group", response, history, http.StatusOK)
			if !reflect.DeepEqual(group, history) {
				t.Fatal("trace and child history disagree about failed logical slots")
			}
			response, page, _ := d.do(d.alice, "GET", "/api/swarms/"+d.swarm+"/children", "", nil)
			expectStatus(t, "failed job child summary", response, page, http.StatusOK)
			summary := page["items"].([]any)[0].(map[string]any)
			if summary["unregisteredFailed"] != float64(1) || summary["registered"] != float64(0) || summary["open"] != float64(1) {
				t.Fatalf("metadata page lost failure accounting: %+v", summary)
			}
			job := traceEphemeralJob(t, body)
			failure, ok := job["failure"].(map[string]any)
			if !ok || !reflect.DeepEqual(failure, map[string]any{"reason": reason, "failedAt": retained.FailedAt}) {
				t.Fatalf("human trace lost exact server decision or conflated it with child result: %+v", job)
			}
			for _, absent := range []string{"childId", "attestation", "resultRetention", "outcome", "report", "state"} {
				if job[absent] != nil {
					t.Fatalf("pre-registration failure fabricated %s", absent)
				}
			}
			if allocated {
				if job["createAuthorization"].(map[string]any)["state"] != "reserved" || job["inventory"].(map[string]any)["phase"] != "provisioning" {
					t.Fatal("logical failure erased may-have-issued physical hold")
				}
			} else if job["allocation"] != nil || job["inventory"] != nil {
				t.Fatal("queued failure invented an executor")
			}
			response, download, _ := d.do(d.alice, "GET", path+"?download=1", "", nil)
			expectStatus(t, "failed job download", response, download, http.StatusOK)
			if !reflect.DeepEqual(body, download) {
				t.Fatal("download changed retained failure")
			}
			bob := newClient(t)
			d.login(bob, "bob", "bob-password-1")
			response, hidden, _ := d.do(bob, "GET", path, "", nil)
			expectStatus(t, "hidden failed job", response, hidden, http.StatusNotFound)
			after, sent := d.calls()
			if len(before) != len(after) || len(messages) != len(sent) {
				t.Fatal("history read triggered actor effects")
			}
			if _, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failure projection created a result seal or lost history access: %v", err)
			}
		})
	}
}
