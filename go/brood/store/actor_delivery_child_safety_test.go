package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// FEATURE: Текущий proof-bound scope обязателен для каждого действия ledger.
// SPEC: Отказ не меняет child, intent, попытки или receipts даже после start.
func TestChildDeliveryEveryOperationRechecksCurrentScope(t *testing.T) {
	changes := []string{"incarnation", "parent-incarnation", "not-ready", "parent-link", "origin", "template", "fingerprint", "pool", "namespace", "project", "stopped", "interrupted", "owner", "owner-settled", "effect", "unknown-effect", "missing-proof", "pending-proof", "unknown-proof", "wrong-proof", "child-role", "child-payload", "cancelled", "missing-allocation"}
	for _, change := range changes {
		for _, prepared := range []bool{false, true} {
			name := change + "/pending"
			if prepared {
				name = change + "/running"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				s, intent, child, in := childDeliveryFixture(t)
				var d ActorDeliveryIntent
				var ticket ActorDeliveryAttempt
				now := time.Now().UTC()
				if prepared {
					var err error
					d, err = prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow)
					mustNil(t, err)
					ticket, err = s.ReserveActorDeliveryAttempt(ctx, d.ID, now, now.Add(time.Second), allow)
					mustNil(t, err)
				}
				exec := func(query string, args ...any) { t.Helper(); _, err := s.db.Exec(query, args...); mustNil(t, err) }
				switch change {
				case "incarnation":
					setActor(t, s, "isolated", "incarnation=2")
				case "parent-incarnation":
					setActor(t, s, "a", "incarnation=2")
				case "not-ready":
					setActor(t, s, "isolated", "phase='stopped'")
				case "parent-link":
					setActor(t, s, "isolated", "parent_uid='b'")
				case "origin":
					setActor(t, s, "isolated", "origin='https://other.example/repo.git'")
				case "template":
					setActor(t, s, "isolated", "template='other'")
				case "fingerprint":
					setActor(t, s, "isolated", "source_fingerprint='other'")
				case "namespace":
					exec(`UPDATE swarms SET atespace_uid='other' WHERE id='one'`)
				case "project":
					seedProject(t, s, "other-project", "Other")
					exec(`UPDATE swarms SET project_id='other-project' WHERE id='one'`)
				case "pool":
					pool, err := s.GetManagedPool(ctx, "pool")
					mustNil(t, err)
					pool.State = "draining"
					_, err = s.UpdateManagedPool(ctx, pool, pool.Revision)
					mustNil(t, err)
				case "stopped":
					sw, err := s.GetSwarm(ctx, "one")
					mustNil(t, err)
					_, err = s.StopSwarm(ctx, "one", sw.OwnerID, allow)
					mustNil(t, err)
				case "interrupted", "owner", "owner-settled":
					uid := "isolated"
					if change == "interrupted" {
						uid = "a"
					}
					actor, err := s.GetManagedActor(ctx, uid)
					mustNil(t, err)
					sw, err := s.GetSwarm(ctx, "one")
					mustNil(t, err)
					op, err := s.AcquireOwnerActorOperation(ctx, actor, "intervene", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
					mustNil(t, err)
					if change == "owner-settled" {
						mustNil(t, s.FinishActorOperation(ctx, op, true))
					}
				case "effect", "unknown-effect":
					// Моделируем retained workspace hold от другой операции; публичный
					// admission уже отказывает при child obligations, но ledger обязан перепроверять его.
					op := ActorOperation{ProjectID: intent.Parent.Principal.ProjectID, SwarmID: in.SwarmID, UID: in.ActorUID, Generation: 1, OperationID: "effect", Caller: "trusted", Operation: "workspace_attest", State: "inflight"}
					exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), op.UID)
					if change == "unknown-effect" {
						mustNil(t, s.FinishActorOperation(ctx, op, false))
					}
				case "missing-proof":
					exec(`DROP TRIGGER ephemeral_child_attestation_delete_guard; DELETE FROM ephemeral_child_attestations`)
				case "pending-proof", "unknown-proof":
					state := "pending"
					if change == "unknown-proof" {
						state = "unconfirmed"
					}
					exec(`DROP TRIGGER ephemeral_child_attestation_update_guard; UPDATE ephemeral_child_attestations SET state=?,attested_origin='',attested_commit=''`, state)
				case "wrong-proof":
					exec(`DROP TRIGGER ephemeral_child_attestation_update_guard; UPDATE ephemeral_child_attestations SET source_commit='other',attested_commit='other'`)
				case "child-role":
					delivery := "queued"
					if prepared {
						delivery = "sending"
					}
					exec(`DROP TRIGGER swarm_child_tasks_fence; UPDATE swarm_child_tasks SET role='worker',delivery=? WHERE id=?`, delivery, child.ID)
				case "child-payload":
					exec(`DROP TRIGGER swarm_child_tasks_fence; UPDATE swarm_child_tasks SET payload='other' WHERE id=?`, child.ID)
				case "cancelled":
					if prepared {
						_, err := s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, 2, "success", "independent report", childAllow)
						mustNil(t, err)
					} else {
						_, err := s.CancelSwarmChild(ctx, intent.Parent, child.RequestID, 1, childAllow)
						mustNil(t, err)
					}
				case "missing-allocation":
					exec(`DROP TRIGGER ephemeral_child_attestation_delete_guard; DELETE FROM ephemeral_child_attestations; DROP TRIGGER ephemeral_child_create_grant_delete_guard; DELETE FROM ephemeral_child_create_grants; DROP TRIGGER ephemeral_child_allocation_delete_guard; DELETE FROM ephemeral_child_allocations`)
				}
				before := databaseRows(t, s.db)
				if got, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow); err == nil || got != (ActorDeliveryIntent{}) {
					t.Fatal("preparation/retry accepted stale scope")
				}
				if prepared {
					if err := s.CheckActorDeliveryCurrent(ctx, d.ID); err == nil {
						t.Fatal("transport fence accepted stale scope")
					}
					if _, err := s.ReserveActorDeliveryAttempt(ctx, d.ID, now.Add(time.Minute), now.Add(2*time.Minute), allow); err == nil {
						t.Fatal("send/probe reservation accepted stale scope")
					}
					if err := s.RecordActorDeliveryOutcome(ctx, ticket, "delivered", "receipt", now, allow); err == nil {
						t.Fatal("receipt accepted stale scope")
					}
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("scope refusal changed durable state")
				}
			})
		}
	}
}

func TestChildDeliveryTransportAuthorityAndBudget(t *testing.T) {
	ctx := context.Background()
	s, intent, _, in := childDeliveryFixture(t)
	d, err := prepareChildDelivery(t, s, ctx, intent.ID, in, childAllow)
	mustNil(t, err)
	now := time.Now().UTC()
	ticket, err := s.ReserveActorDeliveryAttempt(ctx, d.ID, now, now.Add(time.Second), allow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	for _, auth := range []func(context.Context) bool{nil, func(context.Context) bool { return false }} {
		if _, err := s.ReserveActorDeliveryAttempt(ctx, d.ID, now.Add(time.Minute), now.Add(2*time.Minute), auth); err == nil {
			t.Fatal("attempt omitted authority")
		}
		if err := s.RecordActorDeliveryOutcome(ctx, ticket, "delivered", "receipt", now, auth); err == nil {
			t.Fatal("outcome omitted authority")
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("authority denial changed rows")
	}
	for i := 1; i < MaxActorDeliveryAttempts; i++ {
		at := now.Add(time.Duration(i) * time.Hour)
		probe, err := s.ReserveActorDeliveryAttempt(ctx, d.ID, at, at.Add(time.Second), allow)
		mustNil(t, err)
		if probe.Mode != "probe" {
			t.Fatal("unknown send replayed payload")
		}
		if err := s.RecordActorDeliveryOutcome(ctx, probe, "not_sent", "invalid probe evidence", at, allow); err == nil {
			t.Fatal("probe authorized payload retry")
		}
		mustNil(t, s.RecordActorDeliveryOutcome(ctx, probe, "unconfirmed", "unknown", at, allow))
	}
	_, err = s.ReserveActorDeliveryAttempt(ctx, d.ID, now.Add(24*time.Hour), now.Add(24*time.Hour+time.Second), allow)
	if err != ErrActorDeliveryBudget {
		t.Fatalf("budget not enforced: %v", err)
	}
}
