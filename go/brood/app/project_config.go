package app

import "context"

// WithProjectConfiguration runs a change of an existing project's
// configuration in turn with swarm creation and agent admission in the
// project, for a user who sees the project. The operation rechecks its own
// write authority in its transaction.
func (s *Service) WithProjectConfiguration(ctx context.Context, userID, projectID string, operation func() error) error {
	if err := s.requireProjectView(ctx, userID, projectID); err != nil {
		return err
	}
	unlock, err := s.lockProject(ctx, projectID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireProjectView(ctx, userID, projectID); err != nil {
		return err
	}
	return operation()
}

// requireProjectView отличает отсутствующий проект (ErrNotFound) от
// запрета (ErrNotAllowed).
func (s *Service) requireProjectView(ctx context.Context, userID, projectID string) error {
	if err := s.projectExists(ctx, projectID); err != nil {
		return err
	}
	allowed, err := s.Can(ctx, userID, ActionViewProject, ProjectRes(projectID))
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotAllowed
	}
	return nil
}
