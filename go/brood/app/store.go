package app

import "time"

// User — локальный пользователь установки.
// Пароли и сессии — забота слоёв #29/#30, прикладной авторизации нужны
// только флаги активности и административности.
type User struct {
	ID          string
	Username    string
	DisplayName string
	IsAdmin     bool
	Disabled    bool
}

// Project — проект, объединяющий рои.
type Project struct {
	ID   string
	Name string
}

// Membership — членство пользователя в проекте.
type Membership struct {
	ProjectID string
	UserID    string
}

// SwarmState — состояние роя в state machine.
type SwarmState string

const (
	// StateProvisioning — atespace создаётся.
	StateProvisioning SwarmState = "provisioning"
	// StateActive — atespace создан, рой работает.
	StateActive SwarmState = "active"
	// StateDeleting — atespace удаляется.
	StateDeleting SwarmState = "deleting"
	// StateError — последняя операция не удалась, повторяемая.
	StateError SwarmState = "error"
	// StateDeleted — терминальное состояние после удаления.
	StateDeleted SwarmState = "deleted"
)

// Retry-операции, зарезервированные в строке роя после сбоя.
const (
	// RetryCreate — создание atespace не завершилось.
	RetryCreate = "create"
	// RetryDelete — удаление atespace не завершилось.
	RetryDelete = "delete"
)

// Swarm — рой: один atespace Substrate внутри проекта.
type Swarm struct {
	ID           string
	ProjectID    string
	OwnerID      string
	Name         string
	AtespaceName string
	// AtespaceUID — UID созданного atespace; пуст до создания.
	AtespaceUID string
	State       SwarmState
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Retry — незавершённая операция после сбоя (create или delete).
	Retry string
}

// SwarmMember is an explicit local grant within one swarm.
type SwarmMember struct {
	SwarmID string
	UserID  string
	Role    string
}

// Store — минимальный интерфейс хранилища, нужный прикладному слою.
// Таблицы по эпику #27: users, projects, project_members, swarms; сессии
// (#30) прикладному слою не видны. Возвращает данные и молчит; печатают
// только командлеты и HTTP-слой. Реализация #29 удовлетворяет ему
// напрямую или через адаптер.
type Store interface {
	// CreateUser записывает пользователя с готовым ID.
	CreateUser(u User) error
	// UserByID возвращает пользователя по ID; ok=false, если нет.
	UserByID(id string) (User, bool, error)
	// UpdateUser перезаписывает пользователя по ID.
	UpdateUser(u User) error

	// CreateProject записывает проект.
	CreateProject(p Project) error
	// ProjectByID возвращает проект; ok=false, если нет.
	ProjectByID(id string) (Project, bool, error)
	// Projects перечисляет все проекты по имени.
	Projects() ([]Project, error)
	// ProjectsForUser перечисляет проекты членства пользователя.
	ProjectsForUser(userID string) ([]Project, error)

	// AddMembership делает пользователя участником проекта.
	// Повторное добавление существующего членства — ошибка.
	AddMembership(m Membership) error
	// RemoveMembership исключает пользователя из проекта.
	// Отсутствующее членство — ошибка.
	RemoveMembership(projectID, userID string) error
	// IsMember сообщает о членстве пользователя в проекте.
	IsMember(projectID, userID string) (bool, error)
	// MemberIDs перечисляет участников проекта.
	MemberIDs(projectID string) ([]string, error)

	// SaveSwarm записывает нового роя; ID и atespace_name заданы.
	SaveSwarm(s Swarm) error
	// SwarmByID возвращает роя; ok=false, если нет.
	SwarmByID(id string) (Swarm, bool, error)
	// SwarmByAtespaceName ищет роя по имени atespace; ok=false, если нет.
	SwarmByAtespaceName(name string) (Swarm, bool, error)
	// UpdateSwarm меняет только состояние, UID и retry; владелец, проект
	// и имя atespace должны совпадать с текущей записью атомарно.
	UpdateSwarm(s Swarm) error
	// ChangeSwarmOwner атомарно передаёт активный рой от ожидаемого владельца.
	ChangeSwarmOwner(actorID, id, expectedOwnerID, newOwnerID string) error
	// SwarmRole returns an explicit grant; missing grants have ok=false.
	SwarmRole(swarmID, userID string) (role string, ok bool, err error)
	// SwarmMembers lists explicit grants without inferring project access.
	SwarmMembers(swarmID string) ([]SwarmMember, error)
	// SetSwarmMember rechecks actor and target authorization atomically.
	SetSwarmMember(actorID, swarmID, userID, role string) error
	// RemoveSwarmMember rechecks authorization and preserves an owner.
	RemoveSwarmMember(actorID, swarmID, userID string) error
	// SwarmsByMember lists explicitly granted swarms in one project.
	SwarmsByMember(projectID, userID string) ([]Swarm, error)
	// SwarmsByProject перечисляет рои проекта в порядке создания.
	SwarmsByProject(projectID string) ([]Swarm, error)
	// SwarmsByOwner перечисляет рои владельца в проекте.
	SwarmsByOwner(projectID, ownerID string) ([]Swarm, error)
	// Swarms перечисляет все рои в порядке создания.
	Swarms() ([]Swarm, error)
}
