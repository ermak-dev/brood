package app

import "sort"

func (s *memStore) SwarmRole(swarmID, userID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	role, ok := s.grants[[2]string{swarmID, userID}]
	return role, ok, nil
}

func (s *memStore) SwarmMembers(swarmID string) ([]SwarmMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.swarms[swarmID]; !ok {
		return nil, ErrNotFound
	}
	members := []SwarmMember{}
	for key, role := range s.grants {
		if key[0] == swarmID {
			members = append(members, SwarmMember{SwarmID: swarmID, UserID: key[1], Role: role})
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].UserID < members[j].UserID })
	return members, nil
}

func (s *memStore) SwarmsByMember(projectID, userID string) ([]Swarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Swarm{}
	for _, swarm := range s.swarms {
		if _, ok := s.grants[[2]string{swarm.ID, userID}]; ok && swarm.ProjectID == projectID {
			out = append(out, *swarm)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *memStore) SetSwarmMember(actorID, swarmID, userID, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.aclWriteAllowedLocked(actorID, swarmID)
	if err != nil {
		return err
	}
	if role != RoleOwner && role != RoleEditor && role != RoleViewer {
		return ErrInvalidRole
	}
	if !s.validGranteeLocked(current.ProjectID, userID) {
		return ErrNotAllowed
	}
	return s.changeGrantLocked(current, userID, role)
}

func (s *memStore) RemoveSwarmMember(actorID, swarmID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.aclWriteAllowedLocked(actorID, swarmID)
	if err != nil {
		return err
	}
	if _, ok := s.grants[[2]string{swarmID, userID}]; !ok {
		return ErrNotFound
	}
	return s.changeGrantLocked(current, userID, "")
}

func (s *memStore) aclWriteAllowedLocked(actorID, swarmID string) (*Swarm, error) {
	current, ok := s.swarms[swarmID]
	if !ok || current.State == StateDeleted {
		return nil, ErrNotFound
	}
	actor, ok := s.users[actorID]
	if !ok || actor.Disabled {
		return nil, ErrNotAllowed
	}
	if !actor.IsAdmin {
		_, member := s.members[Membership{ProjectID: current.ProjectID, UserID: actorID}]
		if !member || s.grants[[2]string{swarmID, actorID}] != RoleOwner {
			return nil, ErrNotAllowed
		}
	}
	if current.State != StateActive {
		return nil, ErrState
	}
	return current, nil
}

func (s *memStore) validGranteeLocked(projectID, userID string) bool {
	user, exists := s.users[userID]
	_, member := s.members[Membership{ProjectID: projectID, UserID: userID}]
	return exists && !user.Disabled && (user.IsAdmin || member)
}

func (s *memStore) changeGrantLocked(swarm *Swarm, userID, role string) error {
	owners := []string{}
	for key, current := range s.grants {
		if key[0] == swarm.ID && key[1] != userID && current == RoleOwner {
			owners = append(owners, key[1])
		}
	}
	if role == RoleOwner {
		owners = append(owners, userID)
	}
	if len(owners) == 0 {
		return ErrConflict
	}
	if role == "" {
		delete(s.grants, [2]string{swarm.ID, userID})
	} else {
		s.grants[[2]string{swarm.ID, userID}] = role
	}
	if s.grants[[2]string{swarm.ID, swarm.OwnerID}] != RoleOwner {
		sort.Strings(owners)
		swarm.OwnerID = owners[0]
	}
	return nil
}
