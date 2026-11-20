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

// SwarmState — состояние роя: живой или tombstone.
type SwarmState string

const (
	// StateActive — рой рабочий.
	StateActive SwarmState = "active"
	// StateDeleted — удалён; строка остаётся как запись.
	StateDeleted SwarmState = "deleted"
)

// Swarm — группа воркеров проекта и scope шины обмена. Доступ к рою
// есть у его владельца и у активного администратора.
type Swarm struct {
	ID        string
	ProjectID string
	OwnerID   string
	Name      string
	State     SwarmState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store — зависимость сервиса: хранилище пользователей, проектов и роёв.
type Store interface {
	CreateUser(u User) error
	UserByID(id string) (User, bool, error)
	UpdateUser(u User) error

	CreateProject(p Project) error
	ProjectByID(id string) (Project, bool, error)
	ProjectCapacity(projectID string) (ShardCapacity, error)
	SetProjectCapacity(actorID, projectID string, capacity ShardCapacity) error
	Projects() ([]Project, error)
	ProjectsForUser(userID string) ([]Project, error)

	AddMembership(m Membership) error
	RemoveMembership(projectID, userID string) error
	IsMember(projectID, userID string) (bool, error)
	MemberIDs(projectID string) ([]string, error)

	SaveSwarm(s Swarm) error
	SwarmByID(id string) (Swarm, bool, error)
	UpdateSwarm(s Swarm) error
	ChangeSwarmOwner(actorID, id, expectedOwnerID, newOwnerID string) error
	SwarmsByProject(projectID string) ([]Swarm, error)
	SwarmsByOwner(projectID, ownerID string) ([]Swarm, error)
	Swarms() ([]Swarm, error)
}
