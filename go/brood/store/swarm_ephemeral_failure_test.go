package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// FEATURE: Явный серверный отказ до регистрации завершает только логическое обязательство.
// SPEC: FULL, mandatory authority, immutable reason; нет executor/proof/result/cleanup,
// capacity и workspace остаются заняты независимо от логического join.
// IMPLEMENTATION: Immutable failure fact, отдельный typed snapshot и FULL local API.
// RESULT: IMPLEMENTED — runtime RED до API; mixed seal и reason marker также проверены RED→GREEN.
// STATUS: DONE
func recordUnregisteredFailure(s *Store, ctx context.Context, id, reason string, auth SwarmChildAuthorize) error {
	return s.RecordEphemeralChildFailure(ctx, id, reason, auth)
}

func TestEphemeralFailureQueuedAndExactSlot(t *testing.T) {
	ctx := context.Background()
	s, job := allocationFixture(t, 1)
	before := databaseRows(t, s.db)
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 || a.Action != "fail_ephemeral" || a.Parent != job.Parent || a.Caller != job.Parent.Principal || a.RequestID != job.RequestID || a.Payload != job.Payload || a.Executor != (SwarmChildPrincipal{}) {
			t.Fatalf("wrong trusted failure authorization: %+v sync=%d", a, mode)
		}
		return nil
	}
	reason := "Server rejected the local bootstrap plan."
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, reason, auth))
	after := databaseRows(t, s.db)
	delete(after, "ephemeral_child_failures")
	delete(before, "ephemeral_child_failures")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failure invented execution/proof or changed physical history")
	}
	j, err := s.GetSwarmChildJoin(ctx, job.Parent, childAllow)
	mustNil(t, err)
	if len(j.Children) != 0 || j.Evaluation.State != "ready_fail" || j.Evaluation.Open != 0 {
		t.Fatalf("failure did not settle one logical slot honestly: %+v", j)
	}
	report := ChildJoinReport(j)
	if !strings.Contains(report, "failed before registration") || !strings.Contains(report, reason) || strings.Contains(report, "\nChild ") {
		t.Fatalf("failure masqueraded as an independent child report: %q", report)
	}
	queued, err := s.QueuedEphemeralChildIntents(ctx, 0)
	mustNil(t, err)
	if len(queued) != 0 {
		t.Fatal("failed job remained dispatchable")
	}
	stable := databaseRows(t, s.db)
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, reason, auth))
	if calls != 2 || !reflect.DeepEqual(stable, databaseRows(t, s.db)) {
		t.Fatal("retry changed the fact or skipped current authorization")
	}
	if !errors.Is(recordUnregisteredFailure(s, ctx, job.ID, reason+" ", childAllow), ErrSwarmChildConflict) {
		t.Fatal("changed reason replaced immutable fact")
	}
	_, err = s.EnqueueEphemeralChild(ctx, job.Parent, j.Policy, "replacement", "Retry implicitly", "pool", childAllow)
	if err == nil {
		t.Fatal("failure freed a Total slot for implicit retry")
	}
	_, err = s.EnqueueEphemeralChild(ctx, job.Parent, j.Policy, job.RequestID, job.Payload, "pool", childAllow)
	mustNil(t, err)
	_, err = s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	if !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("failed job allocated work: %v", err)
	}
	if err := s.CompletePipelineActorVisit(ctx, job.Parent.Principal.SwarmID, job.Parent.Principal.SwarmID, job.Parent.Sequence, job.Parent.RequestID, job.Parent.Principal.ActorUID, job.Parent.Principal.Incarnation, "fail", report); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("generic completion bypassed server join: %v", err)
	}
	_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
	mustNil(t, err)
	visit, err := s.GetSwarmStep(ctx, job.Parent.Principal.SwarmID, job.Parent.Sequence)
	mustNil(t, err)
	if visit.State != "completed" || visit.Outcome == nil || *visit.Outcome != "fail" || visit.Report != report {
		t.Fatalf("join did not apply exact computed failure: %+v", visit)
	}
	if err := s.SealEphemeralChildResult(ctx, job.ID, childAllow); err == nil {
		t.Fatal("unregistered failure became an external result seal")
	}
}

func TestEphemeralFailureAllocatedBeforeProofRetainsPhysicalUnknown(t *testing.T) {
	ctx := context.Background()
	s, job := ephemeralAttestationFixture(t, true)
	_, err := s.ReserveEphemeralChildAttestation(ctx, job.ID, "unknown-proof", childAllow)
	mustNil(t, err)
	mustNil(t, s.UnconfirmEphemeralChildAttestation(ctx, job.ID, "unknown-proof", childAllow))
	before := databaseRows(t, s.db)
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, "Local server decision to abandon this job; provider outcome remains unknown.", childAllow))
	after := databaseRows(t, s.db)
	delete(before, "ephemeral_child_failures")
	delete(after, "ephemeral_child_failures")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("logical failure released or rewrote physical uncertainty")
	}
	_, err = s.CurrentActorOperation(ctx, "isolated")
	mustNil(t, err)
	for name, action := range map[string]func() error{
		"allocate": func() error {
			_, e := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
			return e
		},
		"attest": func() error {
			_, e := s.ReserveEphemeralChildAttestation(ctx, job.ID, "unknown-proof", childAllow)
			return e
		},
		"record proof": func() error {
			return s.RecordEphemeralChildAttestation(ctx, job.ID, "unknown-proof", job.Origin, job.SourceCommit, childAllow)
		},
		"register": func() error { _, e := s.RegisterEphemeralChild(ctx, job.ID, childAllow); return e },
		"start":    func() error { _, e := s.BeginEphemeralChildStart(ctx, job.ID, childAllow); return e },
		"create":   func() error { _, e := s.ReserveEphemeralChildCreate(ctx, job.ID, childAllow); return e },
		"cleanup":  func() error { _, e := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); return e },
	} {
		if action() == nil {
			t.Fatalf("%s began new work for failed intent", name)
		}
	}
	_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
	mustNil(t, err)
	_, err = s.CurrentActorOperation(ctx, "isolated")
	mustNil(t, err)
	actor, err := s.GetManagedActor(ctx, "isolated")
	mustNil(t, err)
	if actor.Phase != "running" || actor.Desired != "running" {
		t.Fatal("join reclaimed allocated capacity")
	}
}

func TestEphemeralFailureMixedRealGroups(t *testing.T) {
	for _, tc := range []struct {
		mode              string
		quorum, successes int
		state             string
	}{
		{"all", 0, 2, "ready_fail"}, {"any", 0, 1, "ready_success"}, {"any", 0, 0, "ready_fail"}, {"quorum", 2, 2, "ready_success"}, {"quorum", 2, 1, "ready_fail"},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.mode, tc.successes), func(t *testing.T) {
			ctx := context.Background()
			s, p, e := childFixture(t, tc.mode, 3, tc.quorum)
			policy := SwarmChildJoinPolicy{Mode: tc.mode, Total: 3, Quorum: tc.quorum, Remaining: "retain"}
			job, err := s.EnqueueEphemeralChild(ctx, p, policy, "failed-job", "Unregistered task", "pool", childAllow)
			mustNil(t, err)
			mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, "Explicit local rejection", childAllow))
			j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if j.Evaluation != (SwarmChildJoinEvaluation{State: "pending", Open: 2}) {
				t.Fatalf("missing members became ready: %+v", j.Evaluation)
			}
			for n := 0; n < 2; n++ {
				c := reserveChild(t, s, p, e, fmt.Sprintf("real-%d", n))
				c, err = s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
				mustNil(t, err)
				outcome := "fail"
				if n < tc.successes {
					outcome = "success"
				}
				_, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, outcome, "Independent child result", childAllow)
				mustNil(t, err)
			}
			j, err = s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if len(j.Children) != 2 || j.Evaluation != (SwarmChildJoinEvaluation{State: tc.state, Open: 0}) {
				t.Fatalf("mixed membership counted incorrectly: %+v", j)
			}
			_, err = s.JoinSwarmChildren(ctx, p, childAllow)
			mustNil(t, err)
		})
	}
}

func TestEphemeralFailureAtomicityAndAuthority(t *testing.T) {
	for _, kind := range []string{"nil", "deny", "cancel", "ignored", "nested", "durable-nested", "blank", "oversize", "invalid-utf8", "stale-parent", "stale-namespace", "stale-source", "stale-project", "stale-incarnation", "intervention"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, job := allocationFixture(t, 1)
			before := databaseRows(t, s.db)
			reason := "Local server failure decision"
			auth := SwarmChildAuthorize(childAllow)
			switch kind {
			case "nil":
				auth = nil
			case "deny":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				auth = func(context.Context, SwarmChildAuthorization) error { cancel(); return nil }
			case "ignored":
				_, err := s.db.Exec(`CREATE TRIGGER ignore_failure BEFORE INSERT ON ephemeral_child_failures BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			case "blank":
				reason = " \n"
			case "oversize":
				reason = strings.Repeat("x", 1025)
			case "invalid-utf8":
				reason = string([]byte{0xff})
			case "stale-parent":
				setActor(t, s, job.Parent.Principal.ActorUID, "desired='stopped'")
				before = databaseRows(t, s.db)
			case "stale-namespace":
				_, err := s.db.Exec(`UPDATE swarms SET atespace_uid='replaced' WHERE id=?`, job.Parent.Principal.SwarmID)
				mustNil(t, err)
				before = databaseRows(t, s.db)
			case "stale-source":
				_, err := s.db.Exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git' WHERE swarm_id='one'`)
				mustNil(t, err)
				before = databaseRows(t, s.db)
			case "stale-project":
				_, err := s.db.Exec(`UPDATE swarms SET project_id='two' WHERE id=?`, job.Parent.Principal.SwarmID)
				mustNil(t, err)
				before = databaseRows(t, s.db)
			case "stale-incarnation":
				setActor(t, s, job.Parent.Principal.ActorUID, "incarnation=2")
				before = databaseRows(t, s.db)
			case "intervention":
				a, err := s.GetManagedActor(ctx, job.Parent.Principal.ActorUID)
				mustNil(t, err)
				sw, err := s.GetSwarm(ctx, a.SwarmID)
				mustNil(t, err)
				_, err = s.AcquireOwnerActorOperation(ctx, a, "failure-test", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				mustNil(t, err)
				before = databaseRows(t, s.db)
			}
			var err error
			if kind == "nested" || kind == "durable-nested" {
				tx := s.WithTx
				if kind == "durable-nested" {
					tx = s.WithDurableTx
				}
				mustNil(t, tx(ctx, func(ctx context.Context) error {
					err = recordUnregisteredFailure(s, ctx, job.ID, reason, auth)
					return nil
				}))
			} else {
				err = recordUnregisteredFailure(s, ctx, job.ID, reason, auth)
			}
			if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatalf("%s returned success or changed rows: %v", kind, err)
			}
		})
	}
}

func TestEphemeralFailureConcurrentAndImmutable(t *testing.T) {
	ctx := context.Background()
	s, job := allocationFixture(t, 1)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- recordUnregisteredFailure(s, ctx, job.ID, "Same local decision", childAllow)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM ephemeral_child_failures WHERE intent_id=?`, job.ID).Scan(&count))
	if count != 1 {
		t.Fatal("concurrent retries duplicated facts")
	}
	before := databaseRows(t, s.db)
	for _, q := range []string{`UPDATE ephemeral_child_failures SET reason='changed' WHERE intent_id=?`, `DELETE FROM ephemeral_child_failures WHERE intent_id=?`, `INSERT OR REPLACE INTO ephemeral_child_failures SELECT * FROM ephemeral_child_failures WHERE intent_id=?`} {
		if _, err := s.db.Exec(q, job.ID); err == nil {
			t.Fatal("failure history was mutable")
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("immutable refusal changed rows")
	}
}

func TestEphemeralFailureRegisteredChildRefused(t *testing.T) {
	ctx := context.Background()
	s, job := ephemeralAttestationFixture(t, true)
	attestForRegistration(t, s, job)
	_, err := s.RegisterEphemeralChild(ctx, job.ID, childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if err := recordUnregisteredFailure(s, ctx, job.ID, "Not a pre-registration failure", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("registered child failed through wrong API: %v", err)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("registered child was modified")
	}
}

func TestEphemeralFailureBoundedUTF8Report(t *testing.T) {
	ctx := context.Background()
	s, p, _ := childFixture(t, "all", MaxSwarmChildren, 0)
	policy := SwarmChildJoinPolicy{Mode: "all", Total: MaxSwarmChildren, Remaining: "retain"}
	for n := 0; n < MaxSwarmChildren; n++ {
		job, err := s.EnqueueEphemeralChild(ctx, p, policy, fmt.Sprintf("job-%d", n), "Payload", "pool", childAllow)
		mustNil(t, err)
		mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, strings.Repeat("ж", 512), childAllow))
	}
	j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	text := ChildJoinReport(j)
	if j.Evaluation.Open != 0 || len(text) > MaxStepReportBytes || !utf8.ValidString(text) || strings.Count(text, "failed before registration") != MaxSwarmChildren || !strings.Contains(text, "reason truncated; read job history") || strings.Contains(text, "read child status") {
		t.Fatalf("failure report lost identities or byte bounds: %d bytes", len(text))
	}
}
