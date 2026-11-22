// Gateway связывает прикладной слой с SQLite и переводит ошибки хранилища.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// errMemberExists — повторное добавление существующего членства.
var errMemberExists = errors.New("already a member")

// Gateway — адаптер *store.Store к интерфейсу app.Store. Перечисления
// проектов и всех роёв идут через каталог (явный SELECT), остальное —
// методами store.
type Gateway struct {
	store   *store.Store
	catalog *Catalog
}

// StoreGateway оборачивает хранилище и каталог интерфейсом прикладного
// слоя.
func StoreGateway(st *store.Store, catalog *Catalog) *Gateway {
	return &Gateway{store: st, catalog: catalog}
}

func (g *Gateway) CreateUser(u app.User) error {
	return g.store.CreateUser(context.Background(), store.User{
		ID: u.ID, Username: u.Username, DisplayName: nilIfEmpty(u.DisplayName),
		PasswordHash: "-", IsAdmin: u.IsAdmin, Disabled: u.Disabled,
	})
}

func (g *Gateway) UserByID(id string) (app.User, bool, error) {
	u, err := g.store.GetUser(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return app.User{}, false, nil
	}
	if err != nil {
		return app.User{}, false, err
	}
	return toAppUser(u), true, nil
}

func (g *Gateway) UpdateUser(u app.User) error {
	// Прикладной слой меняет только disabled, не устаревшие права и пароль.
	err := g.store.SetUserDisabled(context.Background(), u.ID, u.Disabled)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: user not found", app.ErrNotFound)
	}
	return err
}

func (g *Gateway) CreateProject(p app.Project) error {
	return g.store.CreateProject(context.Background(), store.Project{ID: p.ID, Name: p.Name})
}

func (g *Gateway) ProjectByID(id string) (app.Project, bool, error) {
	p, err := g.store.GetProject(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return app.Project{}, false, nil
	}
	if err != nil {
		return app.Project{}, false, err
	}
	return app.Project{ID: p.ID, Name: p.Name}, true, nil
}

func (g *Gateway) Projects() ([]app.Project, error) {
	rows, err := g.catalog.Projects(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]app.Project, 0, len(rows))
	for _, p := range rows {
		out = append(out, app.Project{ID: p.ID, Name: p.Name})
	}
	return out, nil
}

func (g *Gateway) ProjectsForUser(userID string) ([]app.Project, error) {
	rows, err := g.catalog.ProjectsForUser(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	out := make([]app.Project, 0, len(rows))
	for _, p := range rows {
		out = append(out, app.Project{ID: p.ID, Name: p.Name})
	}
	return out, nil
}

func (g *Gateway) AddMembership(m app.Membership) error {
	err := g.store.AddMember(context.Background(), m.ProjectID, m.UserID)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return fmt.Errorf("%w: user is already a member", errMemberExists)
		}
		if store.IsForeignKeyViolation(err) {
			return fmt.Errorf("%w: project or user does not exist", app.ErrNotFound)
		}
		return err
	}
	return nil
}

func (g *Gateway) RemoveMembership(projectID, userID string) error {
	err := g.store.RemoveMember(context.Background(), projectID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: membership not found", app.ErrNotFound)
	}
	return err
}

func (g *Gateway) IsMember(projectID, userID string) (bool, error) {
	return g.store.IsMember(context.Background(), projectID, userID)
}

func (g *Gateway) MemberIDs(projectID string) ([]string, error) {
	users, err := g.store.MembersOf(context.Background(), projectID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	return ids, nil
}

func (g *Gateway) SaveSwarm(s app.Swarm) error {
	err := g.store.WithTx(context.Background(), func(ctx context.Context) error {
		capacity, err := g.store.GetProjectCapacity(ctx, s.ProjectID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: project not found", app.ErrNotFound)
		}
		if err != nil {
			return err
		}
		rows, err := g.store.ListSwarmsByProject(ctx, s.ProjectID)
		if err != nil {
			return err
		}
		swarms := make([]app.Swarm, 0, len(rows))
		for _, row := range rows {
			swarms = append(swarms, toAppSwarm(row))
		}
		for _, existing := range swarms {
			if existing.State != app.StateDeleted && existing.Name == s.Name {
				return fmt.Errorf("%w: swarm name is already in use", app.ErrExists)
			}
		}
		usage, err := app.ShardUsageFromRecords(s.ProjectID, swarms, nil)
		if err != nil {
			return err
		}
		if err := app.CheckShardCapacity(app.ShardCapacity{MaxSwarms: capacity.MaxSwarms}, usage, app.ShardUsage{Swarms: 1}); err != nil {
			return err
		}
		return g.store.CreateSwarm(ctx, toStoreSwarm(s))
	})
	if store.IsUniqueViolation(err) {
		return fmt.Errorf("%w: swarm name is already in use", app.ErrExists)
	}
	return err
}

func (g *Gateway) SwarmByID(id string) (app.Swarm, bool, error) {
	sw, err := g.store.GetSwarm(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return app.Swarm{}, false, nil
	}
	if err != nil {
		return app.Swarm{}, false, err
	}
	return toAppSwarm(sw), true, nil
}

func (g *Gateway) UpdateSwarm(s app.Swarm) error {
	updated := toStoreSwarm(s)
	// Жизненный цикл никогда не меняет владение, даже из старого снимка.
	return g.store.WithTx(context.Background(), func(ctx context.Context) error {
		current, err := g.store.GetSwarm(ctx, s.ID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: swarm not found", app.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if current.OwnerID != updated.OwnerID || current.ProjectID != updated.ProjectID {
			return fmt.Errorf("%w: swarm identity changed", app.ErrConflict)
		}
		if current.State != updated.State {
			return g.store.SetSwarmState(ctx, updated.ID, updated.State)
		}
		return nil
	})
}

// ChangeSwarmOwner передаёт рой: вызывающий — текущий владелец или активный
// администратор, новый владелец — активный участник проекта или администратор.
// Проверки и запись идут в одной транзакции.
func (g *Gateway) ChangeSwarmOwner(actorID, id, expectedOwnerID, newOwnerID string) error {
	return g.store.WithTx(context.Background(), func(ctx context.Context) error {
		current, err := g.store.GetSwarm(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && current.State == "deleted") {
			return fmt.Errorf("%w: swarm not found", app.ErrNotFound)
		}
		if err != nil {
			return err
		}
		actor, err := g.store.GetUser(ctx, actorID)
		if err != nil || actor.Disabled || (!actor.IsAdmin && current.OwnerID != actorID) {
			return app.ErrNotAllowed
		}
		if current.OwnerID != expectedOwnerID {
			return fmt.Errorf("%w: swarm owner changed", app.ErrConflict)
		}
		owner, err := g.store.GetUser(ctx, newOwnerID)
		if err != nil || owner.Disabled {
			return app.ErrNotAllowed
		}
		if !owner.IsAdmin {
			member, err := g.store.IsMember(ctx, current.ProjectID, newOwnerID)
			if err != nil {
				return err
			}
			if !member {
				return app.ErrNotAllowed
			}
		}
		return g.store.SetSwarmOwner(ctx, id, newOwnerID)
	})
}

func (g *Gateway) SwarmsByProject(projectID string) ([]app.Swarm, error) {
	rows, err := g.catalog.Swarms(context.Background())
	if err != nil {
		return nil, err
	}
	var out []app.Swarm
	for _, sw := range rows {
		if sw.ProjectID == projectID {
			out = append(out, toAppSwarmRow(sw))
		}
	}
	return out, nil
}

func (g *Gateway) SwarmsByOwner(projectID, ownerID string) ([]app.Swarm, error) {
	rows, err := g.catalog.Swarms(context.Background())
	if err != nil {
		return nil, err
	}
	var out []app.Swarm
	for _, sw := range rows {
		if sw.ProjectID == projectID && sw.OwnerID == ownerID {
			out = append(out, toAppSwarmRow(sw))
		}
	}
	return out, nil
}

func (g *Gateway) Swarms() ([]app.Swarm, error) {
	rows, err := g.catalog.Swarms(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]app.Swarm, 0, len(rows))
	for _, sw := range rows {
		out = append(out, toAppSwarmRow(sw))
	}
	return out, nil
}

// toAppUser переводит запись хранилища в пользователя прикладного слоя.
func toAppUser(u store.User) app.User {
	display := ""
	if u.DisplayName != nil {
		display = *u.DisplayName
	}
	return app.User{
		ID: u.ID, Username: u.Username, DisplayName: display,
		IsAdmin: u.IsAdmin, Disabled: u.Disabled,
	}
}

// toStoreSwarm переводит роя прикладного слоя в запись хранилища.
func toStoreSwarm(s app.Swarm) store.Swarm {
	return store.Swarm{ID: s.ID, ProjectID: s.ProjectID, OwnerID: s.OwnerID, Name: s.Name, State: string(s.State)}
}

// toAppSwarm переводит запись хранилища в роя прикладного слоя.
func toAppSwarm(sw store.Swarm) app.Swarm {
	result := app.Swarm{ID: sw.ID, ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: sw.Name, State: app.SwarmState(sw.State)}
	if sw.CreatedAt != nil {
		result.CreatedAt, _ = time.Parse(time.RFC3339Nano, *sw.CreatedAt)
	}
	if sw.UpdatedAt != nil {
		result.UpdatedAt, _ = time.Parse(time.RFC3339Nano, *sw.UpdatedAt)
	}
	return result
}

// toAppSwarmRow переводит строку каталога в роя прикладного слоя.
func toAppSwarmRow(sw rowSwarm) app.Swarm {
	return app.Swarm{ID: sw.ID, ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: sw.Name,
		State: app.SwarmState(sw.State), CreatedAt: sw.CreatedAt, UpdatedAt: sw.UpdatedAt}
}

// nilIfEmpty возвращает nil для пустой строки: необязательные поля.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isMemberExists — повторное добавление существующего членства.
func isMemberExists(err error) bool { return errors.Is(err, errMemberExists) }
