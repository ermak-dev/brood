package app

import (
	"errors"
	"fmt"
	"strings"
)

// isNotFound отличает «области нет» от прочих сбоёв провайдера.
func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// AtespaceName строит неизменяемое имя atespace из ID роя:
// "swarm-" + ID без дефисов, lower. Рой с данным ID всегда отображается
// в одно и то же имя, пользователь не может подменить область.
func AtespaceName(swarmID string) string {
	return "swarm-" + strings.ToLower(strings.ReplaceAll(swarmID, "-", ""))
}

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
			return s.resumeCreate(existing)
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

	id := s.newID()
	sw := Swarm{
		ID:           id,
		ProjectID:    projectID,
		OwnerID:      userID,
		Name:         name,
		AtespaceName: AtespaceName(id),
		State:        StateProvisioning,
		CreatedAt:    s.now(),
		UpdatedAt:    s.now(),
	}
	if err := s.store.SaveSwarm(sw); err != nil {
		if errors.Is(err, ErrExists) {
			existing, found, lookupErr := s.findSwarmByName(projectID, name)
			if lookupErr != nil {
				return Swarm{}, lookupErr
			}
			if found && existing.OwnerID == userID {
				return s.resumeCreate(existing)
			}
		}
		return Swarm{}, fmt.Errorf("save swarm: %w", err)
	}
	return s.resumeCreate(sw)
}

// resumeCreate доводит создание atespace до конца и переводит рой
// в active. Сначала сверяет существующую область: имя, занятое чужим
// UID, не присваивается (стойкая строка после сбоя провижининга —
// путь reconcile). Сбой оставляет error с retry-операцией.
func (s *Service) resumeCreate(sw Swarm) (Swarm, error) {
	if sw.State == StateActive {
		return sw, nil
	}
	if sw.State != StateProvisioning && (sw.State != StateError || sw.Retry != RetryCreate) {
		return sw, fmt.Errorf("%w: create from %s (retry %s)", ErrState, sw.State, sw.Retry)
	}
	if existing, err := s.provisioner.GetAtespace(sw.AtespaceName); err == nil {
		if sw.AtespaceUID != "" && existing == sw.AtespaceUID {
			// Наша область уже создана прошлой попыткой.
			sw.Retry = ""
			return s.transition(sw, StateActive)
		}
		// Имя занято областью с другим UID — чужое не присваиваем.
		return s.failSwarm(sw, RetryCreate,
			fmt.Errorf("%w: atespace %s belongs to uid %s", ErrConflict, sw.AtespaceName, existing))
	} else if !isNotFound(err) {
		return s.failSwarm(sw, RetryCreate, err)
	}
	uid, err := s.provisioner.CreateAtespace(sw.AtespaceName)
	if err != nil {
		return s.failSwarm(sw, RetryCreate, err)
	}
	sw.AtespaceUID = uid
	sw.Retry = ""
	return s.transition(sw, StateActive)
}

// DeleteSwarm удаляет рой: владелец или активный админ. Идемпотентно:
// повтор по already-deleted — успех без обращения к Substrate.
// Имя atespace проверяется по сохранённому UID, чужая одноимённая
// область не удаляется. Сбой Substrate оставляет error с retry.
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
		// Повтор идемпотентен только для авторизованного вызывающего.
		return nil
	}
	if guard, ok := s.store.(interface{ CheckSwarmDeletion(string) error }); ok {
		if err := guard.CheckSwarmDeletion(sw.ID); err != nil {
			return err
		}
	}
	switch sw.State {
	case StateActive, StateError:
		sw.Retry = RetryDelete
		sw, err = s.transition(sw, StateDeleting)
		if err != nil {
			return err
		}
	case StateDeleting:
		// Повтор незавершённого удаления тем же путём.
	default:
		return fmt.Errorf("%w: delete from %s", ErrState, sw.State)
	}
	if err := s.provisioner.DeleteAtespace(sw.AtespaceName, sw.AtespaceUID); err != nil {
		uid, gerr := s.provisioner.GetAtespace(sw.AtespaceName)
		if errors.Is(err, ErrSwarmNotEmpty) && gerr == nil && uid != "" && uid == sw.AtespaceUID {
			// Атомарный отказ позволяет откатить только локальное намерение удаления.
			sw.State, sw.Retry, sw.UpdatedAt = StateActive, "", s.now()
			if updateErr := s.store.UpdateSwarm(sw); updateErr != nil {
				return errors.Join(err, updateErr)
			}
			return err
		}
		if !isNotFound(gerr) {
			// Область ещё есть или её состояние неизвестно: фиксируем
			// ошибку с retry. Чужую область с другим UID DeleteAtespace
			// не удаляет сам.
			_, failed := s.failSwarm(sw, RetryDelete, err)
			return failed
		}
		// Области нет — удаление фактически состоялось.
	}
	sw.Retry = ""
	_, err = s.transition(sw, StateDeleted)
	return err
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
	allowed, err := s.canOwnSwarm(user, swarm)
	if err != nil {
		return Swarm{}, err
	}
	if !allowed {
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
	allowed, err := s.canOwnSwarm(actor, sw)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotAllowed
	}
	if sw.State != StateActive {
		return fmt.Errorf("%w: transfer from %s", ErrState, sw.State)
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
		swarms, err = s.store.SwarmsByMember(projectID, userID)
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

// transition переводит роя в новое состояние по state machine
// и перезаписывает строку.
func (s *Service) transition(sw Swarm, to SwarmState) (Swarm, error) {
	if !canTransition(sw.State, to) {
		return sw, fmt.Errorf("%w: %s -> %s", ErrState, sw.State, to)
	}
	sw.State = to
	sw.UpdatedAt = s.now()
	if err := s.store.UpdateSwarm(sw); err != nil {
		return sw, fmt.Errorf("update swarm: %w", err)
	}
	return sw, nil
}

// failSwarm фиксирует сбой: состояние error и зарезервированная
// retry-операция. Возвращает роя и причину сбоя: error-состояние
// не превращает неудачный вызов в успешный.
func (s *Service) failSwarm(sw Swarm, retry string, cause error) (Swarm, error) {
	sw.Retry = retry
	failed, err := s.transition(sw, StateError)
	if err != nil {
		return failed, errors.Join(cause, err)
	}
	return failed, cause
}
