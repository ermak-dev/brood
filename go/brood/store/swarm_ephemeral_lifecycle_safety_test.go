package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// FEATURE: Automatic lifecycle отказывает при устаревшем scope и неизвестном эффекте.
// SPEC: Begin/check/stage требуют текущий точный scope; manual API и исторические
// чтения остаются независимыми от automatic provenance.
// IMPLEMENTATION: Проверки allocation, parent, source, seal и physical binding.
// RESULT: IMPLEMENTED — stale scope и незавершённые доказательства не разрешают automatic effects.
// STATUS: DONE
func TestEphemeralLifecycleScopeFences(t *testing.T) {
	changes := []string{"inc", "parent-inc", "nonce", "resources", "ctid", "cluster", "pool", "template", "fingerprint", "origin", "parent", "namespace", "project", "source", "pass-source", "owner", "parent-owner", "workspace", "parent-workspace", "manual-replacement", "nil", "denied"}
	for _, kind := range []string{"start", "cleanup"} {
		for _, begun := range []bool{false, true} {
			for _, change := range changes {
				t.Run(kind+"/"+map[bool]string{false: "new", true: "existing"}[begun]+"/"+change, func(t *testing.T) {
					ctx := context.Background()
					s, intent := lifecycleFixture(t, kind)
					a, err := s.GetManagedActor(ctx, "isolated")
					mustNil(t, err)
					if begun {
						a, err = beginLifecycle(s, ctx, intent.ID, kind, childAllow)
						mustNil(t, err)
					}
					auth := SwarmChildAuthorize(childAllow)
					exec := func(q string, args ...any) { t.Helper(); _, err := s.db.Exec(q, args...); mustNil(t, err) }
					switch change {
					case "inc":
						setActor(t, s, a.UID, "incarnation=2")
					case "parent-inc":
						setActor(t, s, "a", "incarnation=2")
					case "nonce":
						setActor(t, s, a.UID, "owner_nonce='replacement'")
					case "resources":
						setActor(t, s, a.UID, "cpu_millicores=999")
					case "ctid":
						setActor(t, s, a.UID, "ctid=999")
					case "cluster":
						setActor(t, s, a.UID, "cluster_identity='replacement'")
					case "pool":
						exec(`UPDATE managed_pools SET source_fingerprint='replacement' WHERE uid='pool'`)
					case "template":
						setActor(t, s, a.UID, "template='replacement'")
					case "fingerprint":
						setActor(t, s, a.UID, "source_fingerprint='replacement'")
					case "origin":
						setActor(t, s, a.UID, "origin='https://other.example/repo.git'")
					case "parent":
						setActor(t, s, a.UID, "parent_uid='b'")
					case "namespace":
						exec(`UPDATE swarms SET atespace_uid='replacement' WHERE id='one'`)
					case "project":
						seedProject(t, s, "replacement", "Replacement")
						exec(`UPDATE swarms SET project_id='replacement' WHERE id='one'`)
					case "source":
						exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git' WHERE swarm_id='one'`)
					case "pass-source":
						exec(`DROP TRIGGER swarm_steps_pass_fence; DROP TRIGGER swarm_steps_child_identity`)
						exec(`UPDATE swarm_steps SET source_commit=?,attested_commit=? WHERE swarm_id='one' AND sequence=?`, strings.Repeat("f", 40), strings.Repeat("f", 40), intent.Parent.Pass)
					case "owner", "parent-owner":
						uid, inc := a.UID, a.Incarnation
						if change == "parent-owner" {
							uid, inc = intent.Parent.Principal.ActorUID, intent.Parent.Principal.Incarnation
						}
						// Завершённое вмешательство остаётся automatic fence без активного lease.
						exec(`INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES('intervention',?,?,?,?,'u','human','session_terminal','completed','now')`, intent.Parent.Principal.ProjectID, intent.Parent.Principal.SwarmID, uid, inc)
					case "workspace", "parent-workspace":
						uid, inc := a.UID, a.Incarnation
						if change == "parent-workspace" {
							uid, inc = intent.Parent.Principal.ActorUID, intent.Parent.Principal.Incarnation
						}
						op := ActorOperation{ProjectID: intent.Parent.Principal.ProjectID, SwarmID: intent.Parent.Principal.SwarmID, UID: uid, Generation: inc, OperationID: "unknown-workspace", Caller: "trusted", Operation: "workspace_attest", State: "unconfirmed"}
						exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), op.UID)
					case "manual-replacement":
						if !begun && kind == "cleanup" { // Незавершённый manual delete не заменяется.
							a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "manual-delete", "delete")
							mustNil(t, err)
						} else {
							setActor(t, s, a.UID, "operation_id='manual-replacement'")
						}
					case "nil":
						auth = nil
					case "denied":
						auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
					}
					before := databaseRows(t, s.db)
					out, err := beginLifecycle(s, ctx, intent.ID, kind, auth)
					if err == nil || out != (ManagedActor{}) {
						t.Fatal("stale scope began/replaced automatic lifecycle")
					}
					if begun {
						if err := s.CheckEphemeralChildLifecycle(ctx, a.UID, a.Incarnation, a.OperationID, auth); err == nil {
							t.Fatal("check accepted stale scope")
						}
						if out, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, a.Incarnation, a.OperationID, a.Stage, a.Revision, auth); err == nil || out != (ManagedActor{}) {
							t.Fatal("stale scope reserved effect")
						}
					}
					if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
						t.Fatal("refused scope modified rows")
					}
				})
			}
		}
	}
}

func TestEphemeralLifecycleOriginalCreatePrerequisites(t *testing.T) {
	for _, change := range []string{"queued", "legacy", "missing", "upid", "stage", "phase", "desired", "operation", "operation-id"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, intent := lifecycleFixture(t, "start")
			exec := func(q string) { t.Helper(); _, err := s.db.Exec(q); mustNil(t, err) }
			switch change {
			case "queued", "legacy":
				exec(`DROP TRIGGER ephemeral_child_create_grant_update_guard`)
				exec(`UPDATE ephemeral_child_create_grants SET state='` + change + `',reserved_at=''`)
			case "missing":
				exec(`DROP TRIGGER ephemeral_child_create_grant_delete_guard; DELETE FROM ephemeral_child_create_grants`)
			case "upid":
				setActor(t, s, "isolated", "upid=''")
			case "stage":
				setActor(t, s, "isolated", "stage='create'")
			case "phase":
				setActor(t, s, "isolated", "phase='running'")
			case "desired":
				setActor(t, s, "isolated", "desired='stopped'")
			case "operation":
				setActor(t, s, "isolated", "operation='start'")
			case "operation-id":
				setActor(t, s, "isolated", "operation_id='manual-start'")
			}
			before := databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildStart(ctx, intent.ID, childAllow); err == nil {
				t.Fatal("non-original/unreconciled create obtained first start")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refusal wrote rows")
			}
		})
	}
}

func TestEphemeralLifecycleCleanupRequiresRetainedJoinedResult(t *testing.T) {
	for _, state := range []string{"receipt-only", "report-only", "applied-unsealed", "empty-report"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, intent, child, input := childDeliveryFixture(t)
			delivery, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, input, childAllow)
			mustNil(t, err)
			if state == "receipt-only" {
				now := time.Now().UTC()
				attempt, err := s.ReserveActorDeliveryAttempt(ctx, delivery.ID, now, now.Add(time.Second), allow)
				mustNil(t, err)
				mustNil(t, s.RecordActorDeliveryOutcome(ctx, attempt, "delivered", "confirmed transport receipt, not a result", now, allow))
				mustNil(t, s.ReconcileActorDelivery(ctx, delivery.ID, allow))
			} else {
				report := "independent report"
				if state == "empty-report" {
					report = ""
				}
				_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision+1, "success", report, childAllow)
				mustNil(t, err)
				if state != "report-only" {
					_, err = s.JoinSwarmChildren(ctx, intent.Parent, childAllow)
					mustNil(t, err)
				}
			}
			before := databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildCleanup(ctx, intent.ID, childAllow); err == nil {
				t.Fatal("cleanup accepted absent seal or incomplete evidence")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("cleanup manufactured seal/join/report")
			}
		})
	}
}

func TestEphemeralLifecycleCleanupRevalidatesRetainedEvidence(t *testing.T) {
	for _, change := range []string{"proof", "child-report", "parent-report", "parent-request", "parent-state", "interrupted-join"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, intent := lifecycleFixture(t, "cleanup")
			a, err := s.BeginEphemeralChildCleanup(ctx, intent.ID, childAllow)
			mustNil(t, err)
			exec := func(query string, args ...any) { t.Helper(); _, err := s.db.Exec(query, args...); mustNil(t, err) }
			switch change {
			case "proof":
				exec(`DROP TRIGGER ephemeral_child_attestation_update_guard; UPDATE ephemeral_child_attestations SET state='unconfirmed',attested_origin='',attested_commit=''`)
			case "child-report":
				exec(`DROP TRIGGER swarm_child_tasks_fence; UPDATE swarm_child_tasks SET report='Altered independent report'`)
			case "parent-report":
				exec(`DROP TRIGGER swarm_steps_report_fence`)
				exec(`UPDATE swarm_steps SET report='Altered aggregate' WHERE swarm_id=? AND sequence=?`, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
			case "parent-request":
				exec(`DROP TRIGGER swarm_steps_child_identity`)
				exec(`UPDATE swarm_steps SET request_id='replacement' WHERE swarm_id=? AND sequence=?`, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
			case "parent-state":
				// Повреждённый retained snapshot: снимаем только report write-once
				// fence, сохраняя CHECK, связывающий report с completed outcome.
				exec(`DROP TRIGGER swarm_steps_report_fence`)
				exec(`UPDATE swarm_steps SET state='pending',report=NULL,outcome=NULL WHERE swarm_id=? AND sequence=?`, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
			case "interrupted-join":
				// Завершённый join штатно неизменяем; отдельно проверяем API
				// против повреждённого snapshot, не ослабляя production DDL.
				exec(`DROP TRIGGER swarm_child_joins_immutable; UPDATE swarm_child_joins SET state='aborted',revision=revision+1,aborted_by='u',aborted_at='now'`)
			}
			before := databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildCleanup(ctx, intent.ID, childAllow); err == nil {
				t.Fatal("cleanup retry accepted changed retained evidence")
			}
			if err := s.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, childAllow); err == nil {
				t.Fatal("cleanup check accepted changed evidence")
			}
			if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, childAllow); err == nil {
				t.Fatal("cleanup submitted against changed evidence")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("cleanup repaired or rewrote evidence")
			}
		})
	}
}

func TestEphemeralLifecycleDrainingAndHistoricalCompletedParent(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, intent := lifecycleFixture(t, kind)
			p, err := s.GetManagedPool(ctx, "pool")
			mustNil(t, err)
			p.State = "draining"
			_, err = s.UpdateManagedPool(ctx, p, p.Revision)
			mustNil(t, err)
			if kind == "cleanup" {
				// Настоящий applied Join уже продвинул граф: cleanup проверяет
				// исторического parent, а не новый latest active step.
				var latest int64
				mustNil(t, s.db.QueryRow(`SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, intent.Parent.Principal.SwarmID).Scan(&latest))
				if latest <= intent.Parent.Sequence {
					t.Fatal("fixture did not apply/advance real join")
				}
				a, err := s.BeginEphemeralChildCleanup(ctx, intent.ID, childAllow)
				mustNil(t, err)
				mustNil(t, s.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, childAllow))
				_, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, childAllow)
				mustNil(t, err)
			} else if _, err := s.BeginEphemeralChildStart(ctx, intent.ID, childAllow); err == nil {
				t.Fatal("draining pool allowed new automatic start")
			}
		})
	}
}

func TestEphemeralLifecycleHistorySQLAndManualIndependence(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, intent := lifecycleFixture(t, kind)
			a, err := beginLifecycle(s, ctx, intent.ID, kind, childAllow)
			mustNil(t, err)
			h, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			for _, q := range []string{
				`UPDATE ephemeral_child_lifecycles SET operation_id='other'`,
				`UPDATE ephemeral_child_lifecycles SET kind='cleanup'`,
				`UPDATE ephemeral_child_lifecycles SET created_at='other'`,
				`DELETE FROM ephemeral_child_lifecycles`,
				`INSERT OR REPLACE INTO ephemeral_child_lifecycles SELECT * FROM ephemeral_child_lifecycles`,
				`INSERT INTO ephemeral_child_lifecycles VALUES('other','start','other','now')`,
			} {
				if _, err := s.db.Exec(q); err == nil {
					t.Fatalf("SQL history rewrite accepted: %s", q)
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("SQL refusal changed history")
			}
			_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid=?`, a.UID)
			mustNil(t, err)
			got, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID)
			mustNil(t, err)
			if got != h {
				t.Fatal("inventory removal lost history")
			}
			for _, arg := range []struct {
				uid string
				inc int64
				op  string
			}{{a.UID, 2, a.OperationID}, {"other", 1, a.OperationID}, {a.UID, 1, "other"}} {
				if _, err := s.GetEphemeralChildLifecycle(ctx, arg.uid, arg.inc, arg.op); !errors.Is(err, ErrNotFound) {
					t.Fatal("inexact lookup found history")
				}
			}
			if err := s.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, childAllow); err == nil {
				t.Fatal("history upgraded to live authority")
			}
		})
	}
	// Явный managed start остаётся допустимым без automatic create provenance.
	ctx := context.Background()
	s, _ := lifecycleFixture(t, "start")
	a, err := s.GetManagedActor(ctx, "isolated")
	mustNil(t, err)
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "manual-start", "start")
	mustNil(t, err)
	if _, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatal("manual operation classified automatic")
	}
	if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "start", a.Revision, childAllow); !errors.Is(err, ErrNotFound) {
		t.Fatal("manual operation submitted through automatic gate")
	}
}
