package app

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Shard capacity checks over authoritative resource records (#48).
// SPEC: Missing limits are unlimited; zero is a real limit; suspended actors
// retain reservations, deleted swarms do not; arithmetic must not overflow.
func capacityLimit(value int64) *int64 { return &value }

func TestCheckShardCapacity(t *testing.T) {
	for _, tc := range []struct {
		name            string
		capacity        ShardCapacity
		usage, addition ShardUsage
		field           string
	}{
		{name: "unlimited", usage: ShardUsage{Swarms: 50, Actors: 100, CPUMillicores: 100000, MemoryBytes: 1 << 40}},
		{name: "exact bounds", capacity: ShardCapacity{MaxSwarms: capacityLimit(2), MaxActors: capacityLimit(3), MaxCPUMillicores: capacityLimit(4000), MaxMemoryBytes: capacityLimit(8192)}, usage: ShardUsage{Swarms: 1, Actors: 2, CPUMillicores: 3000, MemoryBytes: 4096}, addition: ShardUsage{Swarms: 1, Actors: 1, CPUMillicores: 1000, MemoryBytes: 4096}},
		{name: "zero swarms", capacity: ShardCapacity{MaxSwarms: capacityLimit(0)}, addition: ShardUsage{Swarms: 1}, field: "swarms"},
		{name: "zero actors", capacity: ShardCapacity{MaxActors: capacityLimit(0)}, addition: ShardUsage{Actors: 1}, field: "actors"},
		{name: "cpu exhausted", capacity: ShardCapacity{MaxCPUMillicores: capacityLimit(4000)}, usage: ShardUsage{CPUMillicores: 3500}, addition: ShardUsage{CPUMillicores: 1000}, field: "cpu_millicores"},
		{name: "memory exhausted", capacity: ShardCapacity{MaxMemoryBytes: capacityLimit(4096)}, usage: ShardUsage{MemoryBytes: 4096}, addition: ShardUsage{MemoryBytes: 1}, field: "memory_bytes"},
		{name: "already over bound", capacity: ShardCapacity{MaxActors: capacityLimit(1)}, usage: ShardUsage{Actors: 2}, field: "actors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckShardCapacity(tc.capacity, tc.usage, tc.addition)
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var exceeded *CapacityExceededError
			if !errors.As(err, &exceeded) || exceeded.Resource != tc.field {
				t.Fatalf("wrong quota refusal: %v", err)
			}
			for _, part := range []string{tc.field, "limit=", "used=", "requested="} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("missing %q in %v", part, err)
				}
			}
		})
	}
}

func TestShardCapacityRejectsInvalidAndOverflowingAmounts(t *testing.T) {
	for _, tc := range []struct {
		capacity        ShardCapacity
		usage, addition ShardUsage
	}{
		{capacity: ShardCapacity{MaxActors: capacityLimit(-1)}},
		{usage: ShardUsage{Actors: -1}},
		{addition: ShardUsage{MemoryBytes: -1}},
		{usage: ShardUsage{CPUMillicores: math.MaxInt64}, addition: ShardUsage{CPUMillicores: 1}},
	} {
		if err := CheckShardCapacity(tc.capacity, tc.usage, tc.addition); err == nil {
			t.Fatalf("accepted invalid amounts: %+v", tc)
		}
	}
}

func TestShardUsageFromRecords(t *testing.T) {
	swarms := []Swarm{{ID: "active", ProjectID: "p", State: StateActive}, {ID: "pending", ProjectID: "p", State: StateProvisioning}, {ID: "error", ProjectID: "p", State: StateError}, {ID: "deleting", ProjectID: "p", State: StateDeleting}, {ID: "deleted", ProjectID: "p", State: StateDeleted}, {ID: "foreign", ProjectID: "other", State: StateActive}}
	actors := []ActorReservation{{SwarmID: "active", CPUMillicores: 1000, MemoryBytes: 2048}, {SwarmID: "active", CPUMillicores: 500, MemoryBytes: 1024}, {SwarmID: "deleting", CPUMillicores: 100, MemoryBytes: 128}, {SwarmID: "deleted", CPUMillicores: 9000, MemoryBytes: 9000}, {SwarmID: "foreign", CPUMillicores: 9000, MemoryBytes: 9000}}
	got, err := ShardUsageFromRecords("p", swarms, actors)
	want := ShardUsage{Swarms: 4, Actors: 3, CPUMillicores: 1600, MemoryBytes: 3200}
	if err != nil || got != want {
		t.Fatalf("usage=%+v, %v; want %+v", got, err, want)
	}
	got, err = ShardUsageFromRecords("empty", nil, nil)
	if err != nil || got != (ShardUsage{}) {
		t.Fatalf("empty usage=%+v, %v", got, err)
	}
	for _, records := range [][]ActorReservation{
		{{SwarmID: "active", MemoryBytes: -1}},
		{{SwarmID: "active", MemoryBytes: math.MaxInt64}, {SwarmID: "active", MemoryBytes: 1}},
	} {
		if _, err := ShardUsageFromRecords("p", swarms, records); err == nil {
			t.Fatal("invalid reservation accepted")
		}
	}
}

func TestShardWorkerSelector(t *testing.T) {
	shard := map[string]string{"shard": "one"}
	for _, tc := range []struct {
		name     string
		explicit map[string]string
		want     map[string]string
		refuse   bool
	}{
		{name: "default", want: map[string]string{"shard": "one"}},
		{name: "empty is default", explicit: map[string]string{}, want: map[string]string{"shard": "one"}},
		{name: "narrower", explicit: map[string]string{"shard": "one", "purpose": "coding"}, want: map[string]string{"shard": "one", "purpose": "coding"}},
		{name: "missing shard", explicit: map[string]string{"purpose": "coding"}, refuse: true},
		{name: "different shard", explicit: map[string]string{"shard": "two"}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ShardWorkerSelector(shard, tc.explicit)
			if tc.refuse {
				if err == nil {
					t.Fatal("accepted selector escaping shard")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selector=%v, %v", got, err)
			}
			got["shard"] = "mutated"
			if shard["shard"] != "one" || tc.explicit["shard"] == "mutated" {
				t.Fatal("selector aliases caller data")
			}
		})
	}
	got, err := ShardWorkerSelector(nil, map[string]string{"purpose": "coding"})
	if err != nil || got["purpose"] != "coding" {
		t.Fatalf("unbound selector=%v, %v", got, err)
	}
}
