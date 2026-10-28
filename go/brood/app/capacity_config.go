package app

import "fmt"

// ShardCapacity returns placement and limits to an active project reader.
func (s *Service) ShardCapacity(userID, projectID string) (ShardCapacity, error) {
	if _, ok, err := s.store.ProjectByID(projectID); err != nil {
		return ShardCapacity{}, err
	} else if !ok {
		return ShardCapacity{}, ErrNotFound
	}
	allowed, err := s.Can(userID, ActionViewProject, ProjectRes(projectID))
	if err != nil {
		return ShardCapacity{}, err
	}
	if !allowed {
		return ShardCapacity{}, ErrNotAllowed
	}
	return s.store.ProjectCapacity(projectID)
}

// SetShardCapacity serializes an administrator's settings change with workload admission.
// Existing reservations are retained when limits are lowered.
func (s *Service) SetShardCapacity(actorID, projectID string, capacity ShardCapacity) error {
	if err := s.requireAdmin(actorID); err != nil {
		return err
	}
	if _, ok, err := s.store.ProjectByID(projectID); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	unlock := s.lockProject(projectID)
	defer unlock()
	if err := s.requireAdmin(actorID); err != nil {
		return err
	}
	if err := CheckShardCapacity(capacity, ShardUsage{}, ShardUsage{}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCapacity, err)
	}
	return s.store.SetProjectCapacity(actorID, projectID, capacity)
}
