package store

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// FEATURE: Failure cleanup не подменяет successful seal и не ослабляет fresh authority.
// SPEC: Mixed join должен быть применён; concurrent begin выдаёт одну operation,
// revocation после begin запрещает reserve, ignored stage writes откатываются.
func TestEphemeralFailureCleanupMixedAppliedJoin(t *testing.T) {
	ctx := context.Background()
	s, failed := allocationFixture(t, 2)
	success, err := s.EnqueueEphemeralChild(ctx, failed.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "successful-job", "Independent result", "pool", childAllow)
	mustNil(t, err)
	for n, job := range []EphemeralChildIntent{failed, success} {
		uid := []string{"failed-worker", "success-worker"}[n]
		addEphemeralCapacity(t, s)
		_, err := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation(uid, 303+n), childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
	}
	mustNil(t, s.RecordEphemeralChildFailure(ctx, failed.ID, "Explicit local bootstrap rejection", childAllow))
	setActor(t, s, "success-worker", "phase='running',stage='done',operation='start'")
	attestForRegistration(t, s, success)
	c, err := s.RegisterEphemeralChild(ctx, success.ID, childAllow)
	mustNil(t, err)
	in := ActorDeliveryInput{SwarmID: c.Executor.SwarmID, SourceKind: "child", SourceID: c.ID, RequestID: c.RequestID, Commit: success.SourceCommit, ActorUID: c.Executor.ActorUID, Incarnation: c.Executor.Incarnation, AtespaceUID: c.Executor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"to":"success-worker","body":"exact instruction","waitSec":0}`}
	_, err = s.PrepareEphemeralChildDelivery(ctx, success.ID, in, childAllow)
	mustNil(t, err)
	_, err = s.CompleteSwarmChild(ctx, success.Parent, c.RequestID, c.Executor, c.Revision+1, "success", "Independent actor result", childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if _, err := s.BeginEphemeralChildCleanup(ctx, failed.ID, childAllow); err == nil {
		t.Fatal("computed but unapplied mixed failure authorized cleanup")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("unapplied begin wrote")
	}
	joined, err := s.JoinSwarmChildren(ctx, failed.Parent, childAllow)
	mustNil(t, err)
	if joined.Evaluation.State != "ready_fail" || len(joined.Children) != 1 || len(joined.UnregisteredFailures) != 1 {
		t.Fatal("incorrect mixed join")
	}
	mustNil(t, s.SealEphemeralChildResult(ctx, success.ID, childAllow))
	for _, job := range []EphemeralChildIntent{failed, success} {
		a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
		mustNil(t, err)
		h, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID)
		mustNil(t, err)
		expected := "cleanup"
		if job.ID == failed.ID {
			expected = "failure_cleanup"
		}
		if h.Kind != expected {
			t.Fatal("mixed join confused cleanup sources")
		}
	}
}

func TestEphemeralFailureCleanupConcurrentBeginAndRevokedStage(t *testing.T) {
	ctx := context.Background()
	s, job := failureCleanupFixture(t, "registered", true)
	var wg sync.WaitGroup
	results := make(chan ManagedActor, 6)
	for range 6 {
		wg.Go(func() {
			a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
			if err != nil {
				t.Error(err)
			}
			results <- a
		})
	}
	wg.Wait()
	close(results)
	var a ManagedActor
	for r := range results {
		if a.UID == "" {
			a = r
		}
		if r != a {
			t.Fatal("concurrent begin minted operations")
		}
	}
	for _, revoke := range []string{"owner", "source", "workspace", "ignore-update"} {
		t.Run(revoke, func(t *testing.T) {
			s, job := failureCleanupFixture(t, "registered", true)
			a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
			mustNil(t, err)
			auth := SwarmChildAuthorize(childAllow)
			switch revoke {
			case "owner":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "source":
				_, err := s.db.Exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git' WHERE swarm_id='one'`)
				mustNil(t, err)
			case "workspace":
				op := ActorOperation{ProjectID: job.Parent.Principal.ProjectID, SwarmID: job.Parent.Principal.SwarmID, UID: a.UID, Generation: 1, OperationID: "unknown-workspace", Caller: "trusted", Operation: "workspace_attest", State: "unconfirmed"}
				_, err := s.db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), op.UID)
				mustNil(t, err)
			case "ignore-update":
				_, err := s.db.Exec(`CREATE TRIGGER ignore_stage BEFORE UPDATE ON managed_actors BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, auth); err == nil {
				t.Fatal("revoked/ignored stage authorized effect")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("failed stage changed state")
			}
		})
	}
}
