package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
)

// BUG: Один native отказ скрывал одновременно DRAINING и несовпадение меток (#90).
// SPEC: Администратору объясняем причины по свежему снимку, обычному участнику
// не читаем и не раскрываем инфраструктуру. Сам отказ всегда остаётся отказом.
func TestResumeFailureExplainsPlacementOnlyToAdministrator(t *testing.T) {
	for _, adminView := range []bool{false, true} {
		t.Run(map[bool]string{false: "member", true: "admin"}[adminView], func(t *testing.T) {
			ts, fake, admin, member, swarm, _ := quotaHTTPFixture(t)
			base := "/api/swarms/" + swarm + "/actors"
			r, b, _ := ts.do(member, "POST", base, ts.csrf(member), map[string]any{"metadata": map[string]string{"name": "blocked"}, "actorTemplate": map[string]string{"name": "base"}})
			expectStatus(t, "create own actor", r, b, http.StatusCreated)
			fake.mu.Lock()
			for _, template := range fake.templates {
				template.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"purpose": "development"}}
			}
			fake.workers["private-worker"] = &ateapi.Worker{Metadata: fakeMeta("private-worker", ""), Status: &ateapi.WorkerStatus{State: ateapi.WorkerState_WORKER_STATE_DRAINING, RegisteredEpoch: 1, SandboxClasses: []*ateapi.WorkerSandboxClass{{SandboxClass: "microvm"}}, Capacity: &ateapi.WorkerResources{CpuMillicores: 24000, MemoryBytes: 1 << 36, Actors: 1000}}}
			fake.resumeError = grpcErr(codes.FailedPrecondition, "no worker has room for the actor")
			fake.mu.Unlock()
			caller := member
			if adminView {
				caller = admin
			}
			r, b, raw := ts.do(caller, "POST", base+"/blocked", ts.csrf(caller), map[string]string{"action": "resume"})
			expectStatus(t, "failed resume", r, b, http.StatusConflict)
			if b["code"] != "actor_placement_unavailable" {
				t.Fatalf("missing stable error code: %v", b)
			}
			fake.mu.Lock()
			reads := fake.workerReads
			fake.mu.Unlock()
			if adminView {
				if reads != 1 || !strings.Contains(raw, "WORKER_STATE_DRAINING") || !strings.Contains(raw, "template selector mismatch") || !strings.Contains(raw, "development") {
					t.Fatalf("incomplete admin diagnostic: reads=%d body=%s", reads, raw)
				}
			} else if reads != 0 || b["placement"] != nil || strings.Contains(raw, "private-worker") || !strings.Contains(raw, "administrator") {
				t.Fatalf("unsafe member diagnostic: reads=%d body=%s", reads, raw)
			}
		})
	}
}

func TestPlacementDoesNotPublishDetailsAfterAdminDemotion(t *testing.T) {
	ts, fake, admin, member, swarm, _ := quotaHTTPFixture(t)
	base := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(member, "POST", base, ts.csrf(member), map[string]any{"metadata": map[string]string{"name": "blocked"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "create actor", r, b, http.StatusCreated)
	user, err := ts.srv.store.GetUserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	other, err := ts.srv.store.GetUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetUserAdmin(context.Background(), other.ID, true); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.resumeError = grpcErr(codes.FailedPrecondition, "no worker has room for the actor")
	fake.beforeWorkerList = func() {
		if err := ts.srv.store.SetUserAdmin(context.Background(), user.ID, false); err != nil {
			t.Error(err)
		}
	}
	fake.mu.Unlock()
	r, b, _ = ts.do(admin, "POST", base+"/blocked", ts.csrf(admin), map[string]string{"action": "resume"})
	expectStatus(t, "failed resume", r, b, http.StatusConflict)
	if b["placement"] != nil {
		t.Fatalf("demoted administrator received fleet details: %v", b)
	}
}

func TestPlacementListingFailureIsNotAnEmptyFleet(t *testing.T) {
	ts, fake, admin, member, swarm, _ := quotaHTTPFixture(t)
	base := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(member, "POST", base, ts.csrf(member), map[string]any{"metadata": map[string]string{"name": "blocked"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "create actor", r, b, http.StatusCreated)
	fake.mu.Lock()
	fake.resumeError = grpcErr(codes.FailedPrecondition, "no worker has room for the actor")
	fake.workerReadError = grpcErr(codes.Unavailable, "upstream unavailable")
	fake.mu.Unlock()
	r, b, _ = ts.do(admin, "POST", base+"/blocked", ts.csrf(admin), map[string]string{"action": "resume"})
	expectStatus(t, "failed resume", r, b, http.StatusConflict)
	if b["placement"] != nil || !strings.Contains(b["detail"].(string), "diagnostics are unavailable") {
		t.Fatalf("failed inventory became an empty fleet: %v", b)
	}
}

func TestUnrelatedResumeFailureKeepsNativeReason(t *testing.T) {
	ts, fake, admin, member, swarm, _ := quotaHTTPFixture(t)
	base := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(member, "POST", base, ts.csrf(member), map[string]any{"metadata": map[string]string{"name": "blocked"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "create actor", r, b, http.StatusCreated)
	fake.mu.Lock()
	fake.resumeError = grpcErr(codes.FailedPrecondition, "native runtime unavailable")
	fake.mu.Unlock()
	r, b, _ = ts.do(admin, "POST", base+"/blocked", ts.csrf(admin), map[string]string{"action": "resume"})
	expectStatus(t, "native error", r, b, http.StatusConflict)
	if b["detail"] != "native runtime unavailable" || b["placement"] != nil || b["code"] != nil {
		t.Fatalf("changed unrelated error: %v", b)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.workerReads != 0 {
		t.Fatal("unrelated error listed workers")
	}
}
