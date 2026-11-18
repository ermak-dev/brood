package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// BUG: VMID cursor предыдущего кластера блокирует доступную группу в другом.
// HYPOTHESIS: Общий hint пересекает cluster identity, хотя holds уже изолированы;
// confirmation budget должен оставаться общим, но cursors — cluster-local.
func TestEphemeralGroupCapacityUsesIndependentClusterCursors(t *testing.T) {
	ctx := context.Background()
	d, intents, firstProvider := groupCapacityFixture(t, false)
	firstPool, err := d.srv.store.GetManagedPool(ctx, intents[0].PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := d.srv.managed.registry.pools[firstPool.Connection]
	_, ca := selfSignFake(t, "second-pve-cluster")
	cfg.caPEM = string(ca)
	cfg.endpoint = "https://second-pve.invalid:8006"
	d.srv.managed.registry.pools["second-cluster"] = cfg
	secondPool, err := d.srv.managed.configuredPool("second-cluster")
	if err != nil {
		t.Fatal(err)
	}
	secondPool.UID = store.NewID()
	secondPool, err = d.srv.store.CreateManagedPool(ctx, secondPool)
	if err != nil {
		t.Fatal(err)
	}
	if secondPool.Source.ClusterIdentity == firstPool.Source.ClusterIdentity {
		t.Fatal("fixture must use distinct cluster identities")
	}
	second, err := d.srv.store.EnqueueEphemeralChild(ctx, intents[0].Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-cluster-job", "Independent cluster task", secondPool.UID, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	firstProvider.suggestion = 999999999
	secondProvider := &autoCapacityPVE{managedFakePVE: &managedFakePVE{server: d.srv}, suggestion: 100, occupied: map[int]bool{}}
	d.srv.managed.newClient = func(c proxmox.Config) (proxmoxAPI, error) {
		if c.Endpoint == cfg.endpoint {
			return secondProvider, nil
		}
		return firstProvider, nil
	}
	d.srv.managed.clients = map[string]proxmoxAPI{}
	allocations, cursors, err := d.srv.allocateQueuedEphemeralGroup(ctx, intents[0].ID, nil, 2)
	if err != nil || len(allocations) != 2 {
		t.Fatalf("feasible distinct-cluster group failed admission: %+v %v", allocations, err)
	}
	for _, expected := range []struct {
		id   string
		vmid int
	}{{intents[0].ID, 999999999}, {second.ID, 100}} {
		binding, err := d.srv.store.GetEphemeralChildAllocation(ctx, expected.id)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := d.srv.store.GetManagedActor(ctx, binding.ActorUID)
		if err != nil || actor.CTID != expected.vmid {
			t.Fatalf("cluster selected wrong VMID: %+v %v", actor, err)
		}
	}
	if cursors[firstPool.Source.ClusterIdentity] != 0 || cursors[secondPool.Source.ClusterIdentity] != 101 {
		t.Fatalf("cluster-scoped resume hints crossed identities: %+v", cursors)
	}
	if firstProvider.nextCalls != 1 || secondProvider.nextCalls != 1 || len(firstProvider.checked) != 1 || len(secondProvider.checked) != 1 {
		t.Fatalf("cluster cursor or shared budget was bypassed: first=%v/%d second=%v/%d", firstProvider.checked, firstProvider.nextCalls, secondProvider.checked, secondProvider.nextCalls)
	}
}
