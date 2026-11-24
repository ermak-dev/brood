package app

import (
	"context"
	"errors"

	"brood.dev/go/brood/store"
)

// Action is what a user wants to do with a project or a swarm.
type Action string

const (
	// ActionViewProject sees a project: its members, limits and images.
	ActionViewProject Action = "view-project"
	// ActionViewSwarm sees a swarm and its agents.
	ActionViewSwarm Action = "view-swarm"
	// ActionManageSwarm manages a swarm: its agents, terminals and messages.
	ActionManageSwarm Action = "manage-swarm"
)

// ResourceKind names the kind of object an action applies to.
type ResourceKind string

const (
	// KindProject is a project.
	KindProject ResourceKind = "project"
	// KindSwarm is a swarm.
	KindSwarm ResourceKind = "swarm"
)

// Resource is the object of an access check: a project or a swarm by ID.
type Resource struct {
	Kind ResourceKind
	ID   string
}

// ProjectRes builds a project resource.
func ProjectRes(id string) Resource { return Resource{Kind: KindProject, ID: id} }

// SwarmRes builds a swarm resource.
func SwarmRes(id string) Resource { return Resource{Kind: KindSwarm, ID: id} }

// Can reports whether the user may perform the action on the resource. A
// refusal is (false, nil); an error is an internal failure only.
//
//   - a disabled or unknown user is refused everything, an administrator included;
//   - an active administrator sees every project and every live swarm;
//   - an active project member sees the project;
//   - a live swarm is open to its owner; project membership never opens another swarm.
//
// Управление пользователями, проектами и членством ресурса не имеет: его
// проверяют сами операции (requireAdmin).
func (s *Service) Can(ctx context.Context, userID string, action Action, res Resource) (bool, error) {
	user, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || user.Disabled {
		return false, err
	}
	switch {
	case res.Kind == KindProject && action == ActionViewProject:
		return s.canViewProject(ctx, user, res.ID)
	case res.Kind == KindSwarm && (action == ActionViewSwarm || action == ActionManageSwarm):
		return s.canAccessSwarm(ctx, user, res.ID)
	default:
		return false, nil
	}
}

// canViewProject открывает существующий проект участнику и администратору.
func (s *Service) canViewProject(ctx context.Context, user store.User, projectID string) (bool, error) {
	_, err := s.store.GetProject(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if user.IsAdmin {
		return true, nil
	}
	return s.store.IsMember(ctx, projectID, user.ID)
}

// canAccessSwarm: рой — единственная граница доступа. Живой рой открыт
// владельцу и администратору; ролей внутри роя нет, видеть и управлять
// может один и тот же круг.
func (s *Service) canAccessSwarm(ctx context.Context, user store.User, swarmID string) (bool, error) {
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sw.State != string(StateDeleted) && (user.IsAdmin || sw.OwnerID == user.ID), nil
}
