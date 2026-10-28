package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/app"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// FEATURE: Derive shard reservations from native actors and pending golden work (#49).
// SPEC: Suspended actors reserve resources; asynchronous pending goldens count
// before creation and exactly once afterward; unknown inventory never becomes zero.
func inventoryTemplate(space, name, uid string, mode ateapi.GoldenSnapshotMode) *ateapi.ActorTemplate {
	return &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name, Uid: uid}, Containers: []*ateapi.Container{{Name: "main", Image: "busybox"}}, Resources: &ateapi.ResourceRequirements{Limits: &ateapi.Resources{CpuMillicores: 1000, MemoryBytes: 1024}}, SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: mode}}}
}

func TestNativeCapacityInventoryPendingGoldenAndSuspendedActors(t *testing.T) {
	for _, actualGolden := range []bool{false, true} {
		t.Run(map[bool]string{false: "before golden creation", true: "golden exists"}[actualGolden], func(t *testing.T) {
			fake := newFakeAteapi()
			ts := newSubstrateServer(t, fake)
			_, _, _, id := actorsSetup(t, ts.testServer)
			stored, err := ts.srv.store.GetSwarm(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			sw := toAppSwarm(stored)
			space := sw.AtespaceName
			ordinary := inventoryTemplate(space, "ordinary", "ordinary-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
			pending := inventoryTemplate(space, "pending", "pending-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_UNSPECIFIED)
			ready := inventoryTemplate(space, "ready", "ready-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED)
			ready.Status = &ateapi.ActorTemplateStatus{GoldenSnapshotStatus: &ateapi.GoldenSnapshotStatus{GoldenTag: &ateapi.ObjectRef{Atespace: space, Name: "ready-uid"}}}
			failed := inventoryTemplate(space, "failed", "failed-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED)
			failed.Status = &ateapi.ActorTemplateStatus{GoldenSnapshotStatus: &ateapi.GoldenSnapshotStatus{ErrorMessage: "initialization failed"}}
			fake.mu.Lock()
			for _, template := range []*ateapi.ActorTemplate{ordinary, pending, ready, failed} {
				fake.templates[space+"/"+template.Metadata.Name] = template
			}
			fake.actors[space+"/sleeping"] = &ateapi.Actor{Metadata: fakeMeta("sleeping", space), ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "ordinary"}, Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED}}
			if actualGolden {
				fake.actors[space+"/pending-uid"] = &ateapi.Actor{Metadata: fakeMeta("pending-uid", space), ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "pending"}}
			}
			fake.mu.Unlock()
			reservations, err := nativeShardReservations(context.Background(), ts.srv.ateapi, []app.Swarm{sw})
			if err != nil {
				t.Fatal(err)
			}
			usage, err := app.ShardUsageFromRecords(sw.ProjectID, []app.Swarm{sw}, reservations)
			if err != nil || usage.Actors != 2 || usage.CPUMillicores != 2000 || usage.MemoryBytes != 2048 {
				t.Fatalf("reservations double-counted or omitted work: %+v, %v", usage, err)
			}
		})
	}
}

func TestNativeCapacityInventoryRefusesUnknownResourcesAndBinding(t *testing.T) {
	for _, scenario := range []string{"missing template", "invalid resources", "unknown namespace UID", "foreign namespace UID"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newFakeAteapi()
			ts := newSubstrateServer(t, fake)
			_, _, _, id := actorsSetup(t, ts.testServer)
			stored, err := ts.srv.store.GetSwarm(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			sw := toAppSwarm(stored)
			space := sw.AtespaceName
			fake.mu.Lock()
			fake.actors[space+"/sleeping"] = &ateapi.Actor{Metadata: fakeMeta("sleeping", space), ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "private-template-name"}}
			if scenario != "missing template" {
				template := inventoryTemplate(space, "private-template-name", "private-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
				if scenario == "invalid resources" {
					template.Resources = nil
				}
				fake.templates[space+"/private-template-name"] = template
			}
			if scenario == "foreign namespace UID" {
				fake.atespaces[space].Metadata.Uid = "foreign-uid"
			}
			fake.mu.Unlock()
			if scenario == "unknown namespace UID" {
				sw.AtespaceUID = ""
			}
			if _, err := nativeShardReservations(context.Background(), ts.srv.ateapi, []app.Swarm{sw}); err == nil {
				t.Fatal("unknown inventory accepted as zero")
			}
		})
	}
}
