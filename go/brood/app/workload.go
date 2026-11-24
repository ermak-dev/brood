package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/store"
)

// WorkloadAdmission describes the agents a project already holds and the
// reservation of one agent mutation. Replace names an existing agent of the
// swarm by Name.
type WorkloadAdmission struct {
	Reservations []ActorReservation
	Name         string
	Replace      bool
	// NoAllocation marks a mutation that cannot reserve more:
	// stop, delete, reset or an in-place restart.
	NoAllocation  bool
	CPUMillicores int64
	MemoryBytes   int64
}

// WorkloadOperation resolves the reservations and applies one admitted agent
// mutation. Prepare lists every existing agent once, stopped and pending ones
// included. Apply keeps the UID and revision preconditions of its writes.
type WorkloadOperation interface {
	Prepare(swarms []Swarm) (WorkloadAdmission, error)
	Apply() error
}

// AdmitWorkload checks one agent mutation against the project limits and
// applies it, in turn with other mutations, swarm creation and limit changes
// in the same project of this server. Access is rechecked after Prepare;
// usage is counted, never stored. ctx cancels the wait for the project and
// stops before Apply; Prepare and Apply use the same context for their own
// effects.
func (s *Service) AdmitWorkload(ctx context.Context, userID, swarmID string, operation WorkloadOperation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	swarm, err := s.manageableSwarm(ctx, userID, swarmID)
	if err != nil {
		return err
	}
	unlock, err := s.lockProject(ctx, swarm.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.manageableSwarm(ctx, userID, swarmID); err != nil {
		return err
	}
	rows, err := s.store.ListSwarmsByProject(ctx, swarm.ProjectID)
	if err != nil {
		return err
	}
	swarms := SwarmsOf(rows)
	plan, err := operation.Prepare(swarms)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := s.manageableSwarm(ctx, userID, swarmID)
	if err != nil {
		return err
	}
	if current.ProjectID != swarm.ProjectID {
		return fmt.Errorf("%w: swarm identity changed", ErrConflict)
	}
	if strings.TrimSpace(plan.Name) == "" || (!plan.NoAllocation && (plan.CPUMillicores <= 0 || plan.MemoryBytes <= 0)) {
		return fmt.Errorf("%w: actor name and positive resolved CPU/memory limits are required", ErrInvalidWorkload)
	}
	limits, err := s.store.GetProjectCapacity(ctx, swarm.ProjectID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.NoAllocation {
		return operation.Apply()
	}
	reservations := make([]ActorReservation, 0, len(plan.Reservations))
	found := false
	for _, reservation := range plan.Reservations {
		if reservation.SwarmID == swarmID && reservation.Name == plan.Name {
			if found {
				return fmt.Errorf("%w: duplicate actor reservation", ErrConflict)
			}
			found = true
			if !plan.Replace {
				return fmt.Errorf("%w: actor name is already in use", ErrExists)
			}
			continue
		}
		reservations = append(reservations, reservation)
	}
	if plan.Replace && !found {
		return fmt.Errorf("%w: actor to replace is missing", ErrNotFound)
	}
	usage, err := ProjectUsageOf(swarm.ProjectID, swarms, reservations)
	if err != nil {
		return err
	}
	addition := ProjectUsage{Actors: 1, CPUMillicores: plan.CPUMillicores, MemoryBytes: plan.MemoryBytes}
	if err := checkLimits(store.ProjectCapacity{}, usage, addition); err != nil {
		return err
	}
	proposed := ProjectUsage{Swarms: usage.Swarms, Actors: usage.Actors + 1, CPUMillicores: usage.CPUMillicores + plan.CPUMillicores, MemoryBytes: usage.MemoryBytes + plan.MemoryBytes}
	before, err := ProjectUsageOf(swarm.ProjectID, swarms, plan.Reservations)
	if err != nil {
		return err
	}
	if err := checkLimitsChange(limits, before, proposed); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation.Apply()
}

// manageableSwarm читает рой, которым пользователь вправе управлять.
func (s *Service) manageableSwarm(ctx context.Context, userID, swarmID string) (store.Swarm, error) {
	swarm, err := s.store.GetSwarm(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Swarm{}, fmt.Errorf("%w: swarm not found", ErrNotFound)
	}
	if err != nil {
		return store.Swarm{}, err
	}
	allowed, err := s.Can(ctx, userID, ActionManageSwarm, SwarmRes(swarmID))
	if err != nil {
		return store.Swarm{}, err
	}
	if !allowed {
		return store.Swarm{}, ErrNotAllowed
	}
	return swarm, nil
}
