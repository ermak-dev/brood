package app

import "fmt"

// SetShardActorLimit serializes a partial limit patch with shard admission.
func (s *Service) SetShardActorLimit(actorID, projectID string, actors *int64) error {
	if err := s.requireAdmin(actorID); err != nil {
		return err
	}
	if actors != nil && *actors < 0 {
		return fmt.Errorf("%w: actor limit must be nonnegative", ErrInvalidCapacity)
	}
	unlock := s.lockProject(projectID)
	defer unlock()
	patcher, ok := s.store.(interface {
		SetProjectActorLimit(string, string, *int64) error
	})
	if !ok {
		return fmt.Errorf("project actor-limit patch is unavailable")
	}
	return patcher.SetProjectActorLimit(actorID, projectID, actors)
}
