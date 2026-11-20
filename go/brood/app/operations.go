package app

import "fmt"

// CreateProject создаёт проект. Только активный администратор.
func (s *Service) CreateProject(actorID, name string) (Project, error) {
	if err := s.requireAdmin(actorID); err != nil {
		return Project{}, err
	}
	id := s.newID()
	p := Project{ID: id, Name: name}
	if err := s.store.CreateProject(p); err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// AddMember добавляет пользователя в проект. Только администратор.
func (s *Service) AddMember(actorID, projectID, userID string) error {
	if err := s.requireAdmin(actorID); err != nil {
		return err
	}
	if _, ok, err := s.store.UserByID(userID); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if err := s.store.AddMembership(Membership{ProjectID: projectID, UserID: userID}); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	return nil
}

// RemoveMember исключает пользователя из проекта. Только администратор.
// Рои исключённого не удаляются: он просто теряет доступ.
func (s *Service) RemoveMember(actorID, projectID, userID string) error {
	if err := s.requireAdmin(actorID); err != nil {
		return err
	}
	if err := s.store.RemoveMembership(projectID, userID); err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	return nil
}

// SetUserDisabled блокирует или разблокирует пользователя. Только
// администратор; защита последнего админа и отзыв сессий должны
// выполняться хранилищем атомарно.
func (s *Service) SetUserDisabled(actorID, userID string, disabled bool) error {
	if err := s.requireAdmin(actorID); err != nil {
		return err
	}
	u, ok, err := s.store.UserByID(userID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	u.Disabled = disabled
	if err := s.store.UpdateUser(u); err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return nil
}

// CreateSwarm создаёт рой в проекте. Вызывающий — активный участник
// проекта или администратор; создатель становится владельцем.
// Идемпотентно по имени в проекте: повтор после сбоя провижининга
// продолжает то же создание, а не заводит второго роя.
func (s *Service) CreateSwarm(userID, projectID, name string) (Swarm, error) {
	if _, ok, err := s.store.ProjectByID(projectID); err != nil {
		return Swarm{}, err
	} else if !ok {
		return Swarm{}, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
	}
	if _, _, err := s.authorizeCreateSwarm(userID, projectID); err != nil {
		return Swarm{}, err
	}
	unlock := s.lockProject(projectID)
	defer unlock()
	if _, _, err := s.authorizeCreateSwarm(userID, projectID); err != nil {
		return Swarm{}, err
	}

	// Имя роя уникально в проекте. Незавершённое создание того же
	// владельца продолжается, чужое имя — отказ.
	existing, found, err := s.findSwarmByName(projectID, name)
	if err != nil {
		return Swarm{}, err
	}
	if found {
		if existing.OwnerID == userID {
			return existing, nil
		}
		return Swarm{}, fmt.Errorf("%w: swarm name %s in project", ErrExists, name)
	}

	capacity, err := s.store.ProjectCapacity(projectID)
	if err != nil {
		return Swarm{}, err
	}
	swarms, err := s.store.SwarmsByProject(projectID)
	if err != nil {
		return Swarm{}, err
	}
	usage, err := ShardUsageFromRecords(projectID, swarms, nil)
	if err != nil {
		return Swarm{}, err
	}
	if err := CheckShardCapacity(ShardCapacity{MaxSwarms: capacity.MaxSwarms}, usage, ShardUsage{Swarms: 1}); err != nil {
		return Swarm{}, err
	}

	sw := Swarm{
		ID:        s.newID(),
		ProjectID: projectID,
		OwnerID:   userID,
		Name:      name,
		State:     StateActive,
		CreatedAt: s.now(),
		UpdatedAt: s.now(),
	}
	if err := s.store.SaveSwarm(sw); err != nil {
		return Swarm{}, fmt.Errorf("save swarm: %w", err)
	}
	return sw, nil
}

// DeleteSwarm удаляет рой: владелец или активный админ. Идемпотентно.
// Рой с неудалёнными воркерами хранилище удалить не даёт.
func (s *Service) DeleteSwarm(userID, swarmID string) error {
	sw, err := s.swarmForDeletion(userID, swarmID)
	if err != nil {
		return err
	}
	unlock := s.lockProject(sw.ProjectID)
	defer unlock()
	sw, err = s.swarmForDeletion(userID, swarmID)
	if err != nil {
		return err
	}
	if sw.State == StateDeleted {
		return nil
	}
	if guard, ok := s.store.(interface{ CheckSwarmDeletion(string) error }); ok {
		if err := guard.CheckSwarmDeletion(sw.ID); err != nil {
			return err
		}
	}
	sw.State = StateDeleted
	sw.UpdatedAt = s.now()
	if err := s.store.UpdateSwarm(sw); err != nil {
		return fmt.Errorf("update swarm: %w", err)
	}
	return nil
}

func (s *Service) swarmForDeletion(userID, swarmID string) (Swarm, error) {
	swarm, ok, err := s.store.SwarmByID(swarmID)
	if err != nil {
		return Swarm{}, err
	}
	if !ok {
		return Swarm{}, fmt.Errorf("%w: swarm %s", ErrNotFound, swarmID)
	}
	user, ok, err := s.store.UserByID(userID)
	if err != nil {
		return Swarm{}, err
	}
	if !ok || user.Disabled {
		return Swarm{}, ErrNotAllowed
	}
	if !user.IsAdmin && swarm.OwnerID != userID {
		return Swarm{}, ErrNotAllowed
	}
	return swarm, nil
}

// TransferSwarm changes the primary owner, revoking the previous primary grant.
// An active owner or administrator may transfer to an active member or administrator.
func (s *Service) TransferSwarm(actorID, swarmID, newOwnerID string) error {
	actor, ok, err := s.store.UserByID(actorID)
	if err != nil {
		return err
	}
	if !ok || actor.Disabled {
		return ErrNotAllowed
	}
	sw, ok, err := s.store.SwarmByID(swarmID)
	if err != nil {
		return err
	}
	if !ok || sw.State == StateDeleted {
		return fmt.Errorf("%w: swarm %s", ErrNotFound, swarmID)
	}
	if !actor.IsAdmin && sw.OwnerID != actorID {
		return ErrNotAllowed
	}
	newOwner, ok, err := s.store.UserByID(newOwnerID)
	if err != nil {
		return err
	}
	if !ok || newOwner.Disabled {
		return ErrNotAllowed
	}
	member, err := s.store.IsMember(sw.ProjectID, newOwnerID)
	if err != nil {
		return err
	}
	if !member && !newOwner.IsAdmin {
		return ErrNotAllowed
	}
	if err := s.store.ChangeSwarmOwner(actorID, sw.ID, sw.OwnerID, newOwnerID); err != nil {
		return fmt.Errorf("transfer swarm: %w", err)
	}
	return nil
}

// GetSwarm выдаёт рой по ID с проверкой доступа: владелец-участник или
// активный админ; удалённый рой не существует ни для кого.
func (s *Service) GetSwarm(userID, swarmID string) (Swarm, error) {
	ok, err := s.Can(userID, ActionViewSwarm, SwarmRes(swarmID))
	if err != nil {
		return Swarm{}, err
	}
	if !ok {
		sw, exists, err := s.store.SwarmByID(swarmID)
		if err != nil {
			return Swarm{}, err
		}
		if !exists || sw.State == StateDeleted {
			return Swarm{}, fmt.Errorf("%w: swarm %s", ErrNotFound, swarmID)
		}
		return Swarm{}, ErrNotAllowed
	}
	sw, _, err := s.store.SwarmByID(swarmID)
	return sw, err
}

// ListProjectsFor перечисляет проекты членства, а админу — все проекты.
func (s *Service) ListProjectsFor(userID string) ([]Project, error) {
	u, ok, err := s.store.UserByID(userID)
	if err != nil {
		return nil, err
	}
	if !ok || u.Disabled {
		return nil, ErrNotAllowed
	}
	if u.IsAdmin {
		return s.store.Projects()
	}
	return s.store.ProjectsForUser(userID)
}

// ListSwarmsFor перечисляет собственные рои участника проекта,
// а активному администратору — все неудалённые рои проекта.
func (s *Service) ListSwarmsFor(userID, projectID string) ([]Swarm, error) {
	ok, err := s.Can(userID, ActionViewProject, ProjectRes(projectID))
	if err != nil {
		return nil, err
	}
	if !ok {
		if _, exists, err2 := s.store.ProjectByID(projectID); err2 != nil || !exists {
			return nil, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
		}
		return nil, ErrNotAllowed
	}
	u, exists, err := s.store.UserByID(userID)
	if err != nil {
		return nil, err
	}
	if !exists || u.Disabled {
		return nil, ErrNotAllowed
	}
	var swarms []Swarm
	if u.IsAdmin {
		swarms, err = s.store.SwarmsByProject(projectID)
	} else {
		swarms, err = s.store.SwarmsByOwner(projectID, userID)
	}
	if err != nil {
		return nil, err
	}
	out := swarms[:0]
	for _, sw := range swarms {
		if sw.State != StateDeleted {
			out = append(out, sw)
		}
	}
	return out, nil
}

// AdminSwarms перечисляет все рои установки. Только администратор.
func (s *Service) AdminSwarms(actorID string) ([]Swarm, error) {
	if err := s.requireAdmin(actorID); err != nil {
		return nil, err
	}
	swarms, err := s.store.Swarms()
	if err != nil {
		return nil, err
	}
	out := swarms[:0]
	for _, sw := range swarms {
		if sw.State != StateDeleted {
			out = append(out, sw)
		}
	}
	return out, nil
}

// AdminProjects перечисляет все проекты установки. Только администратор.
func (s *Service) AdminProjects(actorID string) ([]Project, error) {
	if err := s.requireAdmin(actorID); err != nil {
		return nil, err
	}
	return s.store.Projects()
}

// requireAdmin проверяет, что вызывающий — активный администратор.
func (s *Service) requireAdmin(actorID string) error {
	u, ok, err := s.store.UserByID(actorID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotAllowed
	}
	if u.Disabled || !u.IsAdmin {
		return ErrNotAllowed
	}
	return nil
}

// authorizeCreateSwarm проверяет активность пользователя и членство
// в проекте перед созданием роя.
func (s *Service) authorizeCreateSwarm(userID, projectID string) (User, bool, error) {
	u, ok, err := s.store.UserByID(userID)
	if err != nil {
		return User{}, false, err
	}
	if !ok || u.Disabled {
		return User{}, false, ErrNotAllowed
	}
	if u.IsAdmin {
		return u, true, nil
	}
	member, err := s.store.IsMember(projectID, userID)
	if err != nil {
		return User{}, false, err
	}
	if !member {
		return User{}, false, ErrNotAllowed
	}
	return u, true, nil
}

// findSwarmByName ищет незавершённый рой с тем же именем в проекте.
func (s *Service) findSwarmByName(projectID, name string) (Swarm, bool, error) {
	swarms, err := s.store.SwarmsByProject(projectID)
	if err != nil {
		return Swarm{}, false, err
	}
	for _, sw := range swarms {
		if sw.Name == name && sw.State != StateDeleted {
			return sw, true, nil
		}
	}
	return Swarm{}, false, nil
}
