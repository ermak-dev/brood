package app

// WithProjectConfiguration serializes an existing project's configuration with
// workload admission. The operation must recheck transactional write authority.
func (s *Service) WithProjectConfiguration(userID, projectID string, operation func() error) error {
	if _, ok, err := s.store.ProjectByID(projectID); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	allowed, err := s.Can(userID, ActionViewProject, ProjectRes(projectID))
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotAllowed
	}
	unlock := s.lockProject(projectID)
	defer unlock()
	allowed, err = s.Can(userID, ActionViewProject, ProjectRes(projectID))
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotAllowed
	}
	return operation()
}
