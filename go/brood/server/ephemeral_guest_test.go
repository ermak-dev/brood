package server

// FEATURE: Текущий parent ставит ephemeral job в очередь и читает факты (#292).
// SPEC: Настоящий authenticated relay и SQLite, полная ёмкость не мешает enqueue;
// точные retries, закрытая identity, join hold, только реальные факты и ноль effects.
// IMPLEMENTATION: Гостевой adapter вызывает существующий Store с childAuthority;
// status повторно проверяет namespace и точного текущего parent.
// RESULT: IMPLEMENTED — normal suite и focused race без live PVE/model.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func ephemeralSpawnArgs(d *dispatchServer) map[string]any {
	return map[string]any{"parent_sequence": 1, "parent_request_id": d.committed().Request, "request_id": "queued", "task": "Independent task", "total": 2, "pool_uid": d.master.PoolUID}
}
func ephemeralJobData(t *testing.T, r relay.SwarmResponse) map[string]any {
	t.Helper()
	var data map[string]any
	if !r.OK || json.Unmarshal(r.Data, &data) != nil || data["job_id"] == nil {
		t.Fatalf("job response: %+v", r)
	}
	return data
}
func TestEphemeralGuestFullPoolEnqueueRetryAndJoin(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	parent, worker, received := childRelay(t, d)
	pool, err := d.srv.store.GetManagedPool(ctx, d.master.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	pool.MaxActors = 2
	if _, err = d.srv.store.UpdateManagedPool(ctx, pool, pool.Revision); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err = d.srv.store.SetProjectCapacity(ctx, d.projectID(t), store.ProjectCapacity{MaxActors: &zero}); err != nil {
		t.Fatal(err)
	}
	before, err := d.srv.store.ListManagedActors(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	calls, messages := d.calls()
	d.srv.managed.clients = map[string]proxmoxAPI{}
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		t.Error("enqueue reached provider")
		return nil, errors.New("forbidden")
	}
	args := ephemeralSpawnArgs(d)
	first := childCall(t, parent, "child_spawn", args)
	data := ephemeralJobData(t, first)
	if first.Outcome != "accepted" || data["request_id"] != "queued" || data["task"] != args["task"] || data["pool_uid"] != pool.UID || data["queued_at"] == nil || data["child_id"] != nil || data["failure"] != nil || data["state"] != nil {
		t.Fatalf("invented/missing facts: %+v", data)
	}
	id := data["job_id"].(string)
	retry := ephemeralJobData(t, childCall(t, parent, "child_spawn", args))
	if !reflect.DeepEqual(data, retry) {
		t.Fatalf("retry changed job: %+v %+v", data, retry)
	}
	for _, key := range []string{"task", "pool_uid", "parent_request_id", "total"} {
		bad := ephemeralSpawnArgs(d)
		if key == "total" {
			bad[key] = 3
		} else {
			bad[key] = "changed"
		}
		if r := childCall(t, parent, "child_spawn", bad); r.OK || r.Outcome != "refused" {
			t.Fatalf("changed %s accepted: %+v", key, r)
		}
	}
	if r := childCall(t, worker, "child_spawn", args); r.OK {
		t.Fatal("worker enqueued")
	}
	status := ephemeralJobData(t, childCall(t, parent, "child_job", map[string]any{"job_id": id}))
	if !reflect.DeepEqual(data, status) {
		t.Fatalf("read changed facts: %+v", status)
	}
	if r := childCall(t, worker, "child_job", map[string]any{"job_id": id}); r.OK || len(r.Data) != 0 {
		t.Fatalf("worker read job: %+v", r)
	}
	d.srv.children.dispatch(ctx)
	queued, err := d.srv.store.QueuedEphemeralChildIntents(ctx, 0)
	if err != nil || len(queued) != 1 || queued[0].ID != id {
		t.Fatalf("queue: %+v %v", queued, err)
	}
	if _, err = d.srv.store.GetEphemeralChildAllocation(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("allocated job: %v", err)
	}
	join := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": args["parent_request_id"]})
	var j struct {
		Open     int   `json:"open"`
		Joined   bool  `json:"joined"`
		Children []any `json:"children"`
	}
	if !join.OK || json.Unmarshal(join.Data, &j) != nil || j.Open != 2 || j.Joined || len(j.Children) != 0 {
		t.Fatalf("queued join released/invented child: %+v", join)
	}
	if r := childCall(t, parent, "pipeline_visit_complete", map[string]any{"run_id": d.swarm, "sequence": 1, "request_id": args["parent_request_id"], "outcome": "success"}); r.OK {
		t.Fatal("queue bypassed completion hold")
	}
	after, err := d.srv.store.ListManagedActors(ctx, d.swarm)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("enqueue changed actors: %v", err)
	}
	afterCalls, afterMessages := d.calls()
	if len(calls) != len(afterCalls) || len(messages) != len(afterMessages) || received.Load() != 0 {
		t.Fatal("enqueue/status delivered or attested work")
	}
}

func TestEphemeralGuestStatusExactParentAndRealFailure(t *testing.T) {
	for _, scenario := range []string{"failure", "peer", "incarnation", "namespace", "role", "delegation", "foreign-incarnation"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, _ := ephemeralCapacityFixture(t)
			ctx := context.Background()
			parent, worker, _ := childRelay(t, d)
			ephemeralJobData(t, childCall(t, parent, "child_job", map[string]any{"job_id": intent.ID}))
			if scenario == "failure" {
				if err := d.srv.store.RecordEphemeralChildFailure(ctx, intent.ID, "operator_cancelled", d.srv.managed.childAuthority); err != nil {
					t.Fatal(err)
				}
				r := childCall(t, parent, "child_job", map[string]any{"job_id": intent.ID})
				data := ephemeralJobData(t, r)
				failure, ok := data["failure"].(map[string]any)
				if !ok || failure["reason"] != "operator_cancelled" || data["child_id"] != nil || data["state"] != nil || r.Outcome != "completed" {
					t.Fatalf("false execution/failure: %+v", r)
				}
				retry := childCall(t, parent, "child_spawn", map[string]any{"parent_sequence": intent.Parent.Sequence, "parent_request_id": intent.Parent.RequestID, "request_id": intent.RequestID, "task": intent.Payload, "total": 2, "pool_uid": intent.PoolUID})
				if !reflect.DeepEqual(data, ephemeralJobData(t, retry)) {
					t.Fatalf("failed job retry invented work: %+v", retry)
				}
				return
			}
			switch scenario {
			case "peer":
				d.exec(`UPDATE managed_actors SET role='master',delegator_id=? WHERE uid=?`, d.master.DelegatorID, d.worker.UID)
				parent = worker
			case "incarnation":
				d.exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, d.master.UID)
			case "namespace":
				d.exec(`UPDATE managed_actors SET atespace_uid='foreign' WHERE uid=?`, d.master.UID)
			case "role":
				d.bind("build", d.worker.UID)
			case "delegation":
				if err := d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false); err != nil {
					t.Fatal(err)
				}
			case "foreign-incarnation":
				// Неизменяемый job не получает доступ от подменённого current snapshot.
				stale := d.master
				stale.Incarnation++
				r := d.srv.managed.handleActorCommand(ctx, stale, command.Invocation{Name: "child_job", Arguments: json.RawMessage(`{"job_id":"` + intent.ID + `"}`)})
				if r.OK || len(r.Data) != 0 {
					t.Fatalf("foreign incarnation read: %+v", r)
				}
				return
			}
			r := childCall(t, parent, "child_job", map[string]any{"job_id": intent.ID})
			if r.OK || len(r.Data) != 0 {
				t.Fatalf("changed authority read: %+v", r)
			}
			if r := childCall(t, parent, "child_spawn", ephemeralSpawnArgs(d)); r.OK {
				t.Fatalf("changed authority enqueued: %+v", r)
			}
			jobs, err := d.srv.store.QueuedEphemeralChildIntents(ctx, 0)
			if err != nil || len(jobs) != 1 || jobs[0].ID != intent.ID {
				t.Fatalf("denial changed queue: %+v %v", jobs, err)
			}
		})
	}
}

func TestEphemeralGuestStatusRegistrationFacts(t *testing.T) {
	d, intent, child := ephemeralChildServerFixture(t)
	parent, _, _ := childRelay(t, d)
	data := ephemeralJobData(t, childCall(t, parent, "child_job", map[string]any{"job_id": intent.ID}))
	if data["child_id"] != child.ID {
		t.Fatalf("real registration missing: %+v", data)
	}
	raw, _ := json.Marshal(data)
	for _, private := range []string{"ctid", "upid", "nonce", "token", "journal", "hash", "fingerprint", "ownerNonce"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(private)) {
			t.Fatalf("private field %s: %s", private, raw)
		}
	}
}

func TestEphemeralGuestClosingEnvelopeAndPostCommitRevocation(t *testing.T) {
	t.Run("closing-envelope", func(t *testing.T) {
		d := newChildEngineServer(t)
		parent, _, _ := childRelay(t, d)
		args := ephemeralSpawnArgs(d)
		args["task"] = "task " + envelopeClosingTag
		if r := childCall(t, parent, "child_spawn", args); r.OK || r.Outcome != "refused" {
			t.Fatalf("closing envelope: %+v", r)
		}
		jobs, err := d.srv.store.QueuedEphemeralChildIntents(context.Background(), 0)
		if err != nil || len(jobs) != 0 {
			t.Fatalf("refusal enqueued: %+v %v", jobs, err)
		}
	})
	t.Run("post-commit", func(t *testing.T) {
		d := newChildEngineServer(t)
		parent, _, _ := childRelay(t, d)
		db, err := sql.Open("sqlite", d.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var hookErr error
		revoked := false
		d.fake.mu.Lock()
		var observeCommit func()
		observeCommit = func() {
			// Namespace hook одноразовый: повторяем, пока независимое
			// SQLite-соединение не увидит committed intent.
			d.fake.beforeNamespaceGet = observeCommit
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM ephemeral_child_intents`).Scan(&count); err != nil {
				hookErr = err
				return
			}
			if count > 0 && !revoked {
				revoked = true
				hookErr = d.srv.store.SetUserAdmin(context.Background(), d.master.DelegatorID, false)
			}
		}
		d.fake.beforeNamespaceGet = observeCommit
		d.fake.mu.Unlock()
		r := childCall(t, parent, "child_spawn", ephemeralSpawnArgs(d))
		d.fake.mu.Lock()
		d.fake.beforeNamespaceGet = nil
		d.fake.mu.Unlock()
		if hookErr != nil || !revoked || r.OK || r.Outcome != "unconfirmed" || len(r.Data) != 0 {
			t.Fatalf("post-commit false refusal/success: revoked=%v err=%v response=%+v", revoked, hookErr, r)
		}
		jobs, err := d.srv.store.QueuedEphemeralChildIntents(context.Background(), 0)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("committed job lost: %+v %v", jobs, err)
		}
	})
}

func TestEphemeralGuestNodeOwnedPolicy(t *testing.T) {
	for _, policy := range []string{"mode: any", "mode: quorum, quorum: 2"} {
		t.Run(policy, func(t *testing.T) {
			d := newChildEngineServerWithPolicy(t, policy)
			parent, _, _ := childRelay(t, d)
			data := ephemeralJobData(t, childCall(t, parent, "child_spawn", ephemeralSpawnArgs(d)))
			intent, err := d.srv.store.GetEphemeralChildIntent(context.Background(), data["job_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			join, err := d.srv.store.GetSwarmChildJoin(context.Background(), intent.Parent, d.srv.managed.childAuthority)
			if err != nil || join.Policy.Total != 2 || join.Policy.Remaining != "retain" {
				t.Fatalf("join policy: %+v %v", join, err)
			}
			if policy == "mode: any" && join.Policy.Mode != "any" || strings.Contains(policy, "quorum") && (join.Policy.Mode != "quorum" || join.Policy.Quorum != 2) {
				t.Fatalf("guest replaced node policy: %+v", join.Policy)
			}
		})
	}
}

func TestEphemeralGuestNamespaceLocalRechecks(t *testing.T) {
	for _, scenario := range []string{"spawn-local", "spawn-namespace", "status-after-read"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, _ := ephemeralCapacityFixture(t)
			parent, _, _ := childRelay(t, d)
			ephemeralJobData(t, childCall(t, parent, "child_job", map[string]any{"job_id": intent.ID}))
			swarm, err := d.srv.store.GetSwarm(context.Background(), d.swarm)
			if err != nil {
				t.Fatal(err)
			}
			var hookErr error
			revoke := func() { hookErr = d.srv.store.SetUserAdmin(context.Background(), d.master.DelegatorID, false) }
			d.fake.mu.Lock()
			switch scenario {
			case "spawn-local":
				d.fake.beforeNamespaceGet = revoke
			case "spawn-namespace":
				d.fake.beforeNamespaceGet = func() { d.fake.atespaces[swarm.AtespaceName].Metadata.Uid = "replacement" }
			case "status-after-read":
				d.fake.beforeNamespaceGet = func() { d.fake.beforeNamespaceGet = revoke }
			}
			d.fake.mu.Unlock()
			tool, args := "child_spawn", ephemeralSpawnArgs(d)
			if scenario == "status-after-read" {
				tool, args = "child_job", map[string]any{"job_id": intent.ID}
			}
			r := childCall(t, parent, tool, args)
			d.fake.mu.Lock()
			d.fake.beforeNamespaceGet = nil
			d.fake.mu.Unlock()
			if hookErr != nil || r.OK || len(r.Data) != 0 {
				t.Fatalf("recheck leaked or enqueued: %+v %v", r, hookErr)
			}
			jobs, err := d.srv.store.QueuedEphemeralChildIntents(context.Background(), 0)
			if err != nil || len(jobs) != 1 || jobs[0].ID != intent.ID {
				t.Fatalf("recheck mutated queue: %+v %v", jobs, err)
			}
		})
	}
}
