package app

// Action — действие над ресурсом. manage-swarm покрывает операции акторов
// и терминала. Native policy дополнительно требует администратора в Go API.
type Action string

const (
	// ActionViewProject — видеть проект.
	ActionViewProject Action = "view-project"
	// ActionCreateSwarm — создать рой в проекте.
	ActionCreateSwarm Action = "create-swarm"
	// ActionViewSwarm — видеть рой и его данные.
	ActionViewSwarm Action = "view-swarm"
	// ActionManageSwarm — управлять роем: акторы и терминал.
	ActionManageSwarm Action = "manage-swarm"
	// ActionDeleteSwarm — удалить рой.
	ActionDeleteSwarm Action = "delete-swarm"
	// ActionTransferOwnership — передать владение роем.
	ActionTransferOwnership Action = "transfer-ownership"
	// ActionManageUsers — управление пользователями (админ).
	ActionManageUsers Action = "manage-users"
	// ActionManageProjects — управление проектами (админ).
	ActionManageProjects Action = "manage-projects"
	// ActionManageMembership — управление членством (админ).
	ActionManageMembership Action = "manage-membership"
)

// Kind ресурса, к которому применяется действие.
type ResourceKind string

const (
	// KindNone — действие без конкретного ресурса (глобальные действия).
	KindNone ResourceKind = ""
	// KindProject — ресурс-проект.
	KindProject ResourceKind = "project"
	// KindSwarm — ресурс-рой.
	KindSwarm ResourceKind = "swarm"
)

// Resource — объект авторизации: проект или рой по ID.
type Resource struct {
	Kind ResourceKind
	ID   string
}

// NilRes — ресурс для глобальных действий (управление пользователями,
// проектами, членством): проверяется только роль, не объект.
func NilRes() Resource { return Resource{} }

// ProjectRes строит ресурс-проект.
func ProjectRes(id string) Resource { return Resource{Kind: KindProject, ID: id} }

// SwarmRes строит ресурс-рой.
func SwarmRes(id string) Resource { return Resource{Kind: KindSwarm, ID: id} }

// canTransition — state machine роя: разрешённые переходы.
// provisioning -> active | error; active -> deleting | error;
// deleting -> deleted | error; error -> active | deleting | error |
// deleted (повтор операции или отказ); deleted — терминальный.
func canTransition(from, to SwarmState) bool {
	allowed := map[SwarmState][]SwarmState{
		StateProvisioning: {StateActive, StateError},
		StateActive:       {StateDeleting, StateError},
		StateDeleting:     {StateDeleted, StateError},
		StateError:        {StateActive, StateDeleting, StateError, StateDeleted},
		StateDeleted:      {},
	}
	for _, to2 := range allowed[from] {
		if to2 == to {
			return true
		}
	}
	return false
}

// Can решает, разрешено ли пользователю действие над ресурсом.
// Возвращает (решение, ошибка); ошибка — только внутренние сбои,
// запрет — false, nil. Правила эпика #27:
//   - заблокированный пользователь всегда отказан, включая админа;
//   - активный админ управляет пользователями, проектами, членством
//     и всеми активными роями;
//   - активный участник видит проект и создаёт в нём рои;
//   - рой доступен только владельцу (пока он участник проекта) и админу;
//   - членство само по себе чужие рои не открывает.
func (s *Service) Can(userID string, action Action, res Resource) (bool, error) {
	u, ok, err := s.store.UserByID(userID)
	if err != nil || !ok {
		return false, err
	}
	if u.Disabled {
		return false, nil
	}

	switch action {
	case ActionManageUsers, ActionManageProjects, ActionManageMembership:
		// Глобальные административные действия: только активный админ.
		return u.IsAdmin, nil
	}

	switch res.Kind {
	case KindProject:
		return s.canProject(u, action, res.ID)
	case KindSwarm:
		return s.canSwarm(u, action, res.ID)
	default:
		// Действие над ресурсом без ресурса — ошибка вызова, не запрет.
		return false, ErrNotFound
	}
}

// canProject разрешает просмотр и создание роя участнику или админу.
func (s *Service) canProject(u User, action Action, projectID string) (bool, error) {
	if action != ActionViewProject && action != ActionCreateSwarm {
		return false, nil
	}
	if _, ok, err := s.store.ProjectByID(projectID); err != nil || !ok {
		return false, err
	}
	if u.IsAdmin {
		return true, nil
	}
	member, err := s.store.IsMember(projectID, u.ID)
	if err != nil {
		return false, err
	}
	return member, nil
}

// canSwarm решает действия над роем: доступ требует владения И
// членства в проекте роя (или активного админа). Управление и передача
// владения — только active-рою; просмотр — любому состоянию, кроме
// deleted.
func (s *Service) canSwarm(u User, action Action, swarmID string) (bool, error) {
	sw, ok, err := s.store.SwarmByID(swarmID)
	if err != nil || !ok || sw.State == StateDeleted {
		// Удалённый рой не существует ни для кого, включая админа.
		return false, err
	}
	if !u.IsAdmin {
		if sw.OwnerID != u.ID {
			return false, nil
		}
		member, err := s.store.IsMember(sw.ProjectID, u.ID)
		if err != nil {
			return false, err
		}
		if !member {
			return false, nil
		}
	}
	switch action {
	case ActionViewSwarm:
		return true, nil
	case ActionManageSwarm:
		return sw.State == StateActive, nil
	case ActionTransferOwnership:
		return u.IsAdmin && sw.State == StateActive, nil
	case ActionDeleteSwarm:
		return sw.State == StateActive || sw.State == StateError, nil
	default:
		return false, nil
	}
}
