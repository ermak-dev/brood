package app

import (
	"fmt"
	"strings"
)

// WorkloadAdmission describes an authoritative inventory and a proposed actor reservation.
// Replace identifies an existing actor by Name in the authorized swarm.
type WorkloadAdmission struct {
	Reservations []ActorReservation
	Name         string
	Replace      bool
	// TemplateOnly is a golden-disabled publication that cannot allocate an actor.
	TemplateOnly   bool
	CPUMillicores  int64
	MemoryBytes    int64
	WorkerSelector map[string]string
}

// WorkloadOperation resolves native resources and applies one admitted mutation.
// Prepare includes suspended actors and pending golden actors, without double-counting.
// Apply must retain native UID/version preconditions for updates.
type WorkloadOperation interface {
	Prepare(swarms []Swarm) (WorkloadAdmission, error)
	Apply(workerSelector map[string]string) error
}

// AdmitWorkload serializes inspection and mutation within a shard in this server.
// It rechecks authorization after native inspection and does not persist a usage counter.
func (s *Service) AdmitWorkload(userID, swarmID string, operation WorkloadOperation) error {
	swarm, err := s.managedCapacitySwarm(userID, swarmID)
	if err != nil {
		return err
	}
	unlock := s.lockProject(swarm.ProjectID)
	defer unlock()
	if _, err := s.managedCapacitySwarm(userID, swarmID); err != nil {
		return err
	}
	swarms, err := s.store.SwarmsByProject(swarm.ProjectID)
	if err != nil {
		return err
	}
	plan, err := operation.Prepare(swarms)
	if err != nil {
		return err
	}
	current, err := s.managedCapacitySwarm(userID, swarmID)
	if err != nil {
		return err
	}
	if current.ProjectID != swarm.ProjectID || current.AtespaceName != swarm.AtespaceName || current.AtespaceUID != swarm.AtespaceUID {
		return fmt.Errorf("%w: swarm identity changed", ErrConflict)
	}
	if strings.TrimSpace(plan.Name) == "" || (!plan.TemplateOnly && (plan.CPUMillicores <= 0 || plan.MemoryBytes <= 0)) {
		return fmt.Errorf("%w: actor name and positive resolved CPU/memory limits are required", ErrInvalidWorkload)
	}
	capacity, err := s.store.ProjectCapacity(swarm.ProjectID)
	if err != nil {
		return err
	}
	selector, err := ShardWorkerSelector(capacity.WorkerSelector, plan.WorkerSelector)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidWorkload, err)
	}
	if plan.TemplateOnly {
		if plan.Replace {
			return fmt.Errorf("%w: template publication is not an actor replacement", ErrInvalidWorkload)
		}
		return operation.Apply(selector)
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
	usage, err := ShardUsageFromRecords(swarm.ProjectID, swarms, reservations)
	if err != nil {
		return err
	}
	addition := ShardUsage{Actors: 1, CPUMillicores: plan.CPUMillicores, MemoryBytes: plan.MemoryBytes}
	if err := CheckShardCapacity(ShardCapacity{}, usage, addition); err != nil {
		return err
	}
	proposed := ShardUsage{Swarms: usage.Swarms, Actors: usage.Actors + 1, CPUMillicores: usage.CPUMillicores + plan.CPUMillicores, MemoryBytes: usage.MemoryBytes + plan.MemoryBytes}
	before, err := ShardUsageFromRecords(swarm.ProjectID, swarms, plan.Reservations)
	if err != nil {
		return err
	}
	if err := CheckShardCapacityChange(capacity, before, proposed); err != nil {
		return err
	}
	return operation.Apply(selector)
}

func (s *Service) managedCapacitySwarm(userID, swarmID string) (Swarm, error) {
	swarm, ok, err := s.store.SwarmByID(swarmID)
	if err != nil {
		return Swarm{}, err
	}
	if !ok {
		return Swarm{}, ErrNotFound
	}
	allowed, err := s.Can(userID, ActionManageSwarm, SwarmRes(swarmID))
	if err != nil {
		return Swarm{}, err
	}
	if !allowed {
		return Swarm{}, ErrNotAllowed
	}
	return swarm, nil
}
