package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"brood.dev/go/brood/store"
)

// CreateProject creates a project. Only an active administrator may.
func (s *Service) CreateProject(ctx context.Context, actorID, name string) (store.Project, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return store.Project{}, err
	}
	p := store.Project{ID: store.NewID(), Name: name}
	if err := s.store.CreateProject(ctx, p); err != nil {
		return store.Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// AddMember adds a user to a project. Only an active administrator may.
func (s *Service) AddMember(ctx context.Context, actorID, projectID, userID string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	err := s.store.AddMember(ctx, projectID, userID)
	switch {
	case store.IsUniqueViolation(err):
		return fmt.Errorf("%w: the user is already a member of the project", ErrExists)
	case store.IsForeignKeyViolation(err):
		return fmt.Errorf("%w: the project or the user does not exist", ErrNotFound)
	}
	return err
}

// RemoveMember removes a user from a project. Only an active administrator
// may. The user's swarms stay and remain open to the user.
func (s *Service) RemoveMember(ctx context.Context, actorID, projectID, userID string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	err := s.store.RemoveMember(ctx, projectID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: the user is not a member of the project", ErrNotFound)
	}
	return err
}

// ListProjectsFor lists the projects the user is a member of, and every
// project to an active administrator.
func (s *Service) ListProjectsFor(ctx context.Context, userID string) ([]store.Project, error) {
	user, err := s.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user.IsAdmin {
		return s.store.ListProjects(ctx)
	}
	return s.store.ListMemberProjects(ctx, userID)
}

// ListSwarmsFor lists the live swarms of a project that the caller owns,
// and every live swarm of the project to an active administrator. The
// caller must see the project.
func (s *Service) ListSwarmsFor(ctx context.Context, userID, projectID string) ([]Swarm, error) {
	if err := s.projectExists(ctx, projectID); err != nil {
		return nil, err
	}
	user, err := s.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !user.IsAdmin {
		member, err := s.store.IsMember(ctx, projectID, userID)
		if err != nil {
			return nil, err
		}
		if !member {
			return nil, ErrNotAllowed
		}
	}
	rows, err := s.store.ListSwarmsByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out []Swarm
	for _, row := range rows {
		if row.State != string(StateDeleted) && (user.IsAdmin || row.OwnerID == userID) {
			out = append(out, SwarmOf(row))
		}
	}
	return out, nil
}

// GetSwarm returns a live swarm to its owner or an active administrator.
// A deleted swarm does not exist for anyone.
func (s *Service) GetSwarm(ctx context.Context, userID, swarmID string) (Swarm, error) {
	sw, err := s.readSwarm(ctx, swarmID, true)
	if err != nil {
		return Swarm{}, err
	}
	if err := s.requireOwner(ctx, userID, sw); err != nil {
		return Swarm{}, err
	}
	return SwarmOf(sw), nil
}

// CreateSwarm creates a swarm in a project for an active member or an
// administrator; the creator becomes its owner. It is idempotent by name:
// a retry by the same owner returns the existing swarm, a name another
// owner holds is refused. The project limit on swarms is checked in the
// same transaction as the write.
func (s *Service) CreateSwarm(ctx context.Context, userID, projectID, name string) (Swarm, error) {
	if err := s.projectExists(ctx, projectID); err != nil {
		return Swarm{}, err
	}
	if err := s.requireMember(ctx, userID, projectID); err != nil {
		return Swarm{}, err
	}
	unlock, err := s.lockProject(ctx, projectID)
	if err != nil {
		return Swarm{}, err
	}
	defer unlock()
	var created store.Swarm
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		if err := s.requireMember(ctx, userID, projectID); err != nil {
			return err
		}
		rows, err := s.store.ListSwarmsByProject(ctx, projectID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.State == string(StateDeleted) || row.Name != name {
				continue
			}
			if row.OwnerID != userID {
				return fmt.Errorf("%w: swarm name is already in use", ErrExists)
			}
			created = row
			return nil
		}
		limits, err := s.store.GetProjectCapacity(ctx, projectID)
		if err != nil {
			return err
		}
		usage, err := ProjectUsageOf(projectID, SwarmsOf(rows), nil)
		if err != nil {
			return err
		}
		if err := checkLimits(limits, usage, ProjectUsage{Swarms: 1}); err != nil {
			return err
		}
		id := store.NewID()
		if err := s.store.CreateSwarm(ctx, store.Swarm{ID: id, ProjectID: projectID, OwnerID: userID, Name: name, State: string(StateActive)}); err != nil {
			return err
		}
		created, err = s.store.GetSwarm(ctx, id)
		return err
	})
	if store.IsUniqueViolation(err) {
		return Swarm{}, fmt.Errorf("%w: swarm name is already in use", ErrExists)
	}
	if err != nil {
		return Swarm{}, err
	}
	return SwarmOf(created), nil
}

// requireMember пропускает активного участника проекта и администратора.
func (s *Service) requireMember(ctx context.Context, userID, projectID string) error {
	user, err := s.activeUser(ctx, userID)
	if err != nil || user.IsAdmin {
		return err
	}
	member, err := s.store.IsMember(ctx, projectID, userID)
	if err != nil {
		return err
	}
	if !member {
		return ErrNotAllowed
	}
	return nil
}

// DeleteSwarm deletes a swarm for its owner or an active administrator. It
// is idempotent; a swarm that still has agents is refused.
func (s *Service) DeleteSwarm(ctx context.Context, userID, swarmID string) error {
	sw, err := s.readSwarm(ctx, swarmID, false)
	if err != nil {
		return err
	}
	if err := s.requireOwner(ctx, userID, sw); err != nil {
		return err
	}
	unlock, err := s.lockProject(ctx, sw.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.store.WithTx(ctx, func(ctx context.Context) error {
		sw, err := s.readSwarm(ctx, swarmID, false)
		if err != nil {
			return err
		}
		if err := s.requireOwner(ctx, userID, sw); err != nil {
			return err
		}
		if sw.State == string(StateDeleted) {
			return nil
		}
		agents, err := s.store.SwarmHasAgents(ctx, sw.ID)
		if err != nil {
			return err
		}
		if agents {
			return &APIError{Status: http.StatusConflict, Detail: "The swarm still has agents. Delete them on the Agents page first."}
		}
		return s.store.SetSwarmState(ctx, sw.ID, string(StateDeleted))
	})
}

// TransferSwarm hands a live swarm to another owner. The caller is its owner
// or an active administrator; the new owner is an active member of the
// project or an administrator. Checks and the write share one transaction.
func (s *Service) TransferSwarm(ctx context.Context, actorID, swarmID, newOwnerID string) error {
	return s.store.WithTx(ctx, func(ctx context.Context) error {
		sw, err := s.readSwarm(ctx, swarmID, true)
		if err != nil {
			return err
		}
		if err := s.requireOwner(ctx, actorID, sw); err != nil {
			return err
		}
		if err := s.requireMember(ctx, newOwnerID, sw.ProjectID); err != nil {
			return err
		}
		return s.store.SetSwarmOwner(ctx, sw.ID, newOwnerID)
	})
}
