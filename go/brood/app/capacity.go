package app

import (
	"fmt"
	"math"
)

// ShardCapacity defines optional reservation limits and required worker labels.
// Nil limits are unlimited; a non-nil zero prohibits any reservation.
type ShardCapacity struct {
	MaxSwarms        *int64
	MaxActors        *int64
	MaxCPUMillicores *int64
	MaxMemoryBytes   *int64
}

// ShardUsage is the total reservation, independent of actor running state.
type ShardUsage struct {
	Swarms        int64
	Actors        int64
	CPUMillicores int64
	MemoryBytes   int64
}

// ActorReservation records the resource limits of one existing native actor.
type ActorReservation struct {
	Name          string
	SwarmID       string
	CPUMillicores int64
	MemoryBytes   int64
}

// CapacityExceededError identifies the exhausted resource and its reservation.
type CapacityExceededError struct {
	Resource  string
	Limit     int64
	Used      int64
	Requested int64
}

func (e *CapacityExceededError) Error() string {
	return fmt.Sprintf("shard %s capacity exceeded: limit=%d, used=%d, requested=%d", e.Resource, e.Limit, e.Used, e.Requested)
}

// CheckShardCapacity validates a proposed addition without mutating usage.
func CheckShardCapacity(capacity ShardCapacity, usage, addition ShardUsage) error {
	for _, r := range []struct {
		name            string
		limit           *int64
		used, requested int64
	}{
		{"swarms", capacity.MaxSwarms, usage.Swarms, addition.Swarms},
		{"actors", capacity.MaxActors, usage.Actors, addition.Actors},
		{"cpu_millicores", capacity.MaxCPUMillicores, usage.CPUMillicores, addition.CPUMillicores},
		{"memory_bytes", capacity.MaxMemoryBytes, usage.MemoryBytes, addition.MemoryBytes},
	} {
		if r.used < 0 || r.requested < 0 || (r.limit != nil && *r.limit < 0) {
			return fmt.Errorf("shard %s amounts must be nonnegative", r.name)
		}
		if r.limit != nil && (r.used > *r.limit || r.requested > *r.limit-r.used) {
			return &CapacityExceededError{Resource: r.name, Limit: *r.limit, Used: r.used, Requested: r.requested}
		}
		if r.requested > math.MaxInt64-r.used {
			return fmt.Errorf("shard %s reservation overflows int64", r.name)
		}
	}
	return nil
}

// CheckShardCapacityChange allows existing reservations to drain after a limit is lowered.
// Only an increase in a constrained dimension must fit that dimension's limit.
func CheckShardCapacityChange(capacity ShardCapacity, current, proposed ShardUsage) error {
	if err := CheckShardCapacity(ShardCapacity{}, current, ShardUsage{}); err != nil {
		return err
	}
	if err := CheckShardCapacity(ShardCapacity{}, proposed, ShardUsage{}); err != nil {
		return err
	}
	for _, r := range []struct {
		name          string
		limit         *int64
		before, after int64
	}{
		{"swarms", capacity.MaxSwarms, current.Swarms, proposed.Swarms},
		{"actors", capacity.MaxActors, current.Actors, proposed.Actors},
		{"cpu_millicores", capacity.MaxCPUMillicores, current.CPUMillicores, proposed.CPUMillicores},
		{"memory_bytes", capacity.MaxMemoryBytes, current.MemoryBytes, proposed.MemoryBytes},
	} {
		if r.limit == nil {
			continue
		}
		if *r.limit < 0 {
			return fmt.Errorf("shard %s limit must be nonnegative", r.name)
		}
		if r.after > *r.limit && r.after > r.before {
			return &CapacityExceededError{Resource: r.name, Limit: *r.limit, Used: r.before, Requested: r.after - r.before}
		}
	}
	return nil
}

// ShardUsageFromRecords counts non-deleted swarms and all their existing actors.
// Callers supply resolved actor limits, including suspended and golden actors.
func ShardUsageFromRecords(projectID string, swarms []Swarm, actors []ActorReservation) (ShardUsage, error) {
	live := make(map[string]bool)
	var usage ShardUsage
	for _, swarm := range swarms {
		if swarm.ProjectID == projectID && swarm.State != StateDeleted && !live[swarm.ID] {
			live[swarm.ID] = true
			usage.Swarms++
		}
	}
	for _, actor := range actors {
		if !live[actor.SwarmID] {
			continue
		}
		addition := ShardUsage{Actors: 1, CPUMillicores: actor.CPUMillicores, MemoryBytes: actor.MemoryBytes}
		if err := CheckShardCapacity(ShardCapacity{}, usage, addition); err != nil {
			return ShardUsage{}, err
		}
		usage.Actors++
		usage.CPUMillicores += actor.CPUMillicores
		usage.MemoryBytes += actor.MemoryBytes
	}
	return usage, nil
}
