package app

const (
	// RoleOwner can manage grants, workloads, ownership and swarm deletion.
	RoleOwner = "owner"
	// RoleEditor can manage workloads but not local grants or ownership.
	RoleEditor = "editor"
	// RoleViewer can read a swarm without opening a writable terminal.
	RoleViewer = "viewer"
)

// SwarmMembers returns local ACL grants visible to a swarm reader.
func (s *Service) SwarmMembers(userID, swarmID string) ([]SwarmMember, error) {
	allowed, err := s.Can(userID, ActionViewSwarm, SwarmRes(swarmID))
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrNotAllowed
	}
	return s.store.SwarmMembers(swarmID)
}

// SetSwarmMember grants a role to an active project member, or an administrator.
// The store repeats the authorization inside the write transaction.
func (s *Service) SetSwarmMember(actorID, swarmID, userID, role string) error {
	allowed, err := s.Can(actorID, ActionManageSwarmACL, SwarmRes(swarmID))
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotAllowed
	}
	if role != RoleOwner && role != RoleEditor && role != RoleViewer {
		return ErrInvalidRole
	}
	swarm, ok, err := s.store.SwarmByID(swarmID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if err := s.requireSwarmGrantee(swarm.ProjectID, userID); err != nil {
		return err
	}
	return s.store.SetSwarmMember(actorID, swarmID, userID, role)
}

// RemoveSwarmMember revokes an explicit role, never the final owner.
func (s *Service) RemoveSwarmMember(actorID, swarmID, userID string) error {
	allowed, err := s.Can(actorID, ActionManageSwarmACL, SwarmRes(swarmID))
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotAllowed
	}
	return s.store.RemoveSwarmMember(actorID, swarmID, userID)
}

func (s *Service) canOwnSwarm(user User, swarm Swarm) (bool, error) {
	if user.Disabled {
		return false, nil
	}
	if user.IsAdmin {
		return true, nil
	}
	member, err := s.store.IsMember(swarm.ProjectID, user.ID)
	if err != nil || !member {
		return false, err
	}
	role, granted, err := s.store.SwarmRole(swarm.ID, user.ID)
	return granted && role == RoleOwner, err
}

func (s *Service) requireSwarmGrantee(projectID, userID string) error {
	user, ok, err := s.store.UserByID(userID)
	if err != nil {
		return err
	}
	if !ok || user.Disabled {
		return ErrNotAllowed
	}
	member, err := s.store.IsMember(projectID, userID)
	if err != nil {
		return err
	}
	if !member && !user.IsAdmin {
		return ErrNotAllowed
	}
	return nil
}
