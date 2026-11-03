package server

import (
	"testing"

	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// FEATURE: Provider lifecycle remains explicit without inventing native enum values.
// SPEC: Provider phase/desired are authoritative; shared status.state stays a
// recognized native enum for old inventory readers and never implies a checkpoint.
func TestManagedStatusUsesKnownNativeEnum(t *testing.T) {
	for _, phase := range []string{"provisioning", "running", "stopping", "stopped", "deleting", "error"} {
		item := managedActorJSON(store.ManagedActor{Phase: phase}, store.ManagedPool{}, "swarm", "")
		state := item["status"].(map[string]any)["state"].(string)
		if _, ok := ateapi.ActorState_value[state]; !ok {
			t.Fatalf("phase %s invented native status %s", phase, state)
		}
		if phase == "stopped" && state == "ACTOR_STATE_SUSPENDED" {
			t.Fatal("stopped provider falsely claims native memory checkpoint")
		}
	}
}
