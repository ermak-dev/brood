package app

import (
	"fmt"
	"math"

	"brood.dev/go/brood/store"
)

// Лимиты проекта — store.ProjectCapacity (limits в brood.yaml): nil — без
// предела, ноль запрещает любое резервирование. Занятое не хранится, а
// считается заново по роям и агентам при каждой проверке.

// ProjectUsage is what a project holds against its limits: its live swarms
// and every existing agent of them, whatever the agent's state.
type ProjectUsage struct {
	Swarms        int64
	Actors        int64
	CPUMillicores int64
	MemoryBytes   int64
}

// ActorReservation is the CPU and memory share of one existing agent.
type ActorReservation struct {
	Name          string
	SwarmID       string
	CPUMillicores int64
	MemoryBytes   int64
}

// LimitExceededError names the project limit a new swarm or agent would exceed.
type LimitExceededError struct {
	Resource  string
	Limit     int64
	Used      int64
	Requested int64
}

func (e *LimitExceededError) Error() string {
	return fmt.Sprintf("project %s limit exceeded: limit=%d, used=%d, requested=%d", e.Resource, e.Limit, e.Used, e.Requested)
}

// limitDimension — одно измерение лимитов: имя, предел и два сравниваемых
// количества (занятое и добавка или занятое до и после).
type limitDimension struct {
	name  string
	limit *int64
	a, b  int64
}

func limitDimensions(limits store.ProjectCapacity, a, b ProjectUsage) []limitDimension {
	return []limitDimension{
		{"swarms", limits.MaxSwarms, a.Swarms, b.Swarms},
		{"agents", limits.MaxActors, a.Actors, b.Actors},
		{"cpu_millicores", limits.MaxCPUMillicores, a.CPUMillicores, b.CPUMillicores},
		{"memory_bytes", limits.MaxMemoryBytes, a.MemoryBytes, b.MemoryBytes},
	}
}

// checkLimits проверяет, что добавка помещается в лимиты, не меняя занятого.
func checkLimits(limits store.ProjectCapacity, usage, addition ProjectUsage) error {
	for _, d := range limitDimensions(limits, usage, addition) {
		used, requested := d.a, d.b
		if used < 0 || requested < 0 || (d.limit != nil && *d.limit < 0) {
			return fmt.Errorf("project %s amounts must be nonnegative", d.name)
		}
		if d.limit != nil && (used > *d.limit || requested > *d.limit-used) {
			return &LimitExceededError{Resource: d.name, Limit: *d.limit, Used: used, Requested: requested}
		}
		if requested > math.MaxInt64-used {
			return fmt.Errorf("project %s usage overflows int64", d.name)
		}
	}
	return nil
}

// checkLimitsChange оставляет занятое после снижения лимита: предел
// проверяется только у измерения, которое растёт.
func checkLimitsChange(limits store.ProjectCapacity, before, after ProjectUsage) error {
	if err := checkLimits(store.ProjectCapacity{}, before, ProjectUsage{}); err != nil {
		return err
	}
	if err := checkLimits(store.ProjectCapacity{}, after, ProjectUsage{}); err != nil {
		return err
	}
	for _, d := range limitDimensions(limits, before, after) {
		if d.limit == nil {
			continue
		}
		if *d.limit < 0 {
			return fmt.Errorf("project %s limit must be nonnegative", d.name)
		}
		if d.b > *d.limit && d.b > d.a {
			return &LimitExceededError{Resource: d.name, Limit: *d.limit, Used: d.a, Requested: d.b - d.a}
		}
	}
	return nil
}

// ProjectUsageOf counts the live swarms of the project and the reservations
// of their agents. Callers supply every existing agent, stopped and pending
// ones included.
func ProjectUsageOf(projectID string, swarms []Swarm, agents []ActorReservation) (ProjectUsage, error) {
	live := make(map[string]bool)
	var usage ProjectUsage
	for _, swarm := range swarms {
		if swarm.ProjectID == projectID && swarm.State != StateDeleted && !live[swarm.ID] {
			live[swarm.ID] = true
			usage.Swarms++
		}
	}
	for _, agent := range agents {
		if !live[agent.SwarmID] {
			continue
		}
		addition := ProjectUsage{Actors: 1, CPUMillicores: agent.CPUMillicores, MemoryBytes: agent.MemoryBytes}
		if err := checkLimits(store.ProjectCapacity{}, usage, addition); err != nil {
			return ProjectUsage{}, err
		}
		usage.Actors++
		usage.CPUMillicores += agent.CPUMillicores
		usage.MemoryBytes += agent.MemoryBytes
	}
	return usage, nil
}
