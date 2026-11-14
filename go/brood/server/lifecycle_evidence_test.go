package server

// BUG: Ошибка записи задачи стирает доказательство уже принятого намерения (#299).
// HYPOTHESIS: Результат команды строится по пустому actor после отказавшего CAS.
// SOLUTION: Доказательство записывается у commit намерения независимо от результата команды.
// RESULT: FIXED — отказ до намерения, неизвестный commit и принятое намерение различаются явно.
// STATUS: FIXED

import (
	"context"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox"
)

func TestLifecyclePreIntentEvidence(t *testing.T) {
	for _, failure := range []string{"missing release", "revoked session", "intent CAS"} {
		t.Run(failure, func(t *testing.T) {
			ts, _, swarm, before, pve := lifecycleAuthoritySetup(t)
			ctx := context.Background()
			user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			writes := lifecycleProviderWrites(pve)
			action := "stop"
			if failure == "missing release" {
				action = "update"
			}
			if failure == "revoked session" {
				ctx = context.WithValue(ctx, commandSessionAuthorityKey{}, func(context.Context) bool { return false })
			}
			if failure == "intent CAS" {
				clear(ts.srv.managed.clients)
				ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
					return lifecycleInspectHook{proxmoxAPI: pve, afterInspect: func() {
						execDB(t, ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, before.UID)
					}}, nil
				}
			}
			result, evidence, commandErr := ts.srv.executeOperatorCommandWithEvidence(ctx, user.ID, swarm, "user:"+user.ID, coordinationInvocation(action, map[string]any{"name": before.Name}))
			after, err := ts.srv.store.GetManagedActor(context.Background(), before.UID)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "intent CAS" {
				before.Revision++
			}
			if commandErr == nil || evidence != lifecycleNoIntent || result.Outcome != "" || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
				t.Fatalf("pre-intent refusal evidence=%v result=%+v err=%v actor=%+v -> %+v writes=%v -> %v", evidence, result, commandErr, before, after, writes, lifecycleProviderWrites(pve))
			}
		})
	}
}

func TestLifecycleCommitFailureRetainsPossibleIntentEvidence(t *testing.T) {
	ts, _, swarm, before, pve := lifecycleAuthoritySetup(t)
	ctx := context.Background()
	user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// Отложенный FK отказывает именно на commit, после успешного callback.
	execDB(t, ts, `CREATE TABLE lifecycle_commit_failure (actor_uid TEXT REFERENCES managed_actors(uid) DEFERRABLE INITIALLY DEFERRED)`)
	execDB(t, ts, `CREATE TRIGGER lifecycle_commit_failure AFTER UPDATE OF operation_id ON managed_actors WHEN NEW.operation_id <> OLD.operation_id BEGIN INSERT INTO lifecycle_commit_failure VALUES ('missing-actor'); END`)
	writes := lifecycleProviderWrites(pve)
	result, evidence, commandErr := ts.srv.executeOperatorCommandWithEvidence(ctx, user.ID, swarm, "user:"+user.ID, coordinationInvocation("stop", map[string]any{"name": before.Name}))
	after, err := ts.srv.store.GetManagedActor(ctx, before.UID)
	if err != nil {
		t.Fatal(err)
	}
	if commandErr == nil || evidence != lifecycleIntentPossible || result.Outcome != "unknown" || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
		t.Fatalf("commit uncertainty lost: evidence=%v result=%+v err=%v actor=%+v -> %+v writes=%v -> %v", evidence, result, commandErr, before, after, writes, lifecycleProviderWrites(pve))
	}
}

func TestLifecycleUnrecordedStopRetainsIntentEvidence(t *testing.T) {
	for _, action := range []string{"stop", "reset", "update", "start"} {
		t.Run(action, func(t *testing.T) {
			ts, _, swarm, before, pve := lifecycleAuthoritySetup(t)
			if action == "update" {
				ts.srv.artifactsDir = writeGuestArtifacts(t)
				guestReleaseHub(ts, reportsRelease(oldRelease))
			}
			user, err := ts.srv.store.GetUserByUsername(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if action == "start" {
				_, err := ts.srv.executeOperatorCommand(context.Background(), user.ID, swarm, "user:"+user.ID, coordinationInvocation("stop", map[string]any{"name": before.Name}))
				if err != nil {
					t.Fatal(err)
				}
				before = settleActor(t, ts, before.UID, actorStopped)
			}
			writes := lifecycleProviderWrites(pve)
			failBinding := func() {
				execDB(t, ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, before.UID)
			}
			pve.mu.Lock()
			if action == "start" {
				pve.afterStart = failBinding
			} else {
				pve.afterStop = failBinding
			}
			pve.mu.Unlock()
			args := map[string]any{"name": before.Name}
			if action == "reset" {
				args["confirm"] = true
			}
			result, evidence, commandErr := ts.srv.executeOperatorCommandWithEvidence(context.Background(), user.ID, swarm, "user:"+user.ID, coordinationInvocation(action, args))
			after, err := ts.srv.store.GetManagedActor(context.Background(), before.UID)
			if err != nil {
				t.Fatal(err)
			}
			stage, writeIndex := "stop", 2
			if action == "start" {
				stage, writeIndex = "start", 1
			}
			if after.OperationID == before.OperationID || after.Stage != stage || after.UPID != "" || lifecycleProviderWrites(pve)[writeIndex] != writes[writeIndex]+1 {
				t.Fatalf("invalid post-effect proof: before=%+v after=%+v writes=%v -> %v", before, after, writes, lifecycleProviderWrites(pve))
			}
			if result.Outcome != "unknown" || evidence != lifecycleIntentAccepted || commandErr == nil {
				t.Fatalf("accepted intent lost after task CAS failure: result=%+v evidence=%v err=%v", result, evidence, commandErr)
			}
		})
	}
}
