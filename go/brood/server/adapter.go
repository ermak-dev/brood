// Адаптер прикладного слоя к store #29 и ateapi: gateway реализует
// интерфейсы app.Store и app.Provisioner поверх физического хранилища,
// каталога проектов и клиента Substrate. Ошибки домена (уникальность,
// отсутствие записи) переводятся в ошибки прикладного слоя, на которые
// смотрит HTTP-слой.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// errMemberExists — повторное добавление существующего членства.
var errMemberExists = errors.New("already a member")

// errAteapiNotConfiguredText — общий текст отказа не настроенного
// Substrate-адаптера: рой остаётся в error с retry, операция повторяема.
const errAteapiNotConfiguredText = "substrate is not configured: ATEAPI_TOKEN_FILE, ATEAPI_CA_FILE and ATEAPI_ENDPOINT are required"

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
	err := g.store.CreateSwarm(context.Background(), toStoreSwarm(s))
	if store.IsUniqueViolation(err) {
		return fmt.Errorf("%w: swarm name or atespace is already in use", app.ErrExists)
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

func (g *Gateway) SwarmByAtespaceName(name string) (app.Swarm, bool, error) {
	sw, err := g.store.GetSwarmByAtespace(context.Background(), name)
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
	// Проверка снимка и запись используют одну блокировку BEGIN IMMEDIATE.
	// Жизненный цикл никогда не меняет владение, даже из старого снимка.
	return g.store.WithTx(context.Background(), func(ctx context.Context) error {
		current, err := g.store.GetSwarm(ctx, s.ID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: swarm not found", app.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if current.OwnerID != updated.OwnerID || current.ProjectID != updated.ProjectID || current.AtespaceName != updated.AtespaceName {
			return fmt.Errorf("%w: swarm identity changed", app.ErrConflict)
		}
		if uidChanged(current.AtespaceUID, updated.AtespaceUID) {
			if err := g.store.SetSwarmAtespaceUID(ctx, updated.ID, updated.AtespaceUID); err != nil {
				return err
			}
		}
		if current.State != updated.State || current.Retry != updated.Retry {
			if err := g.store.SetSwarmProgress(ctx, updated.ID, updated.State, updated.Retry); err != nil {
				return err
			}
		}
		return nil
	})
}

// ChangeSwarmOwner отделяет передачу владения от переходов жизненного цикла.
// Проверки и запись атомарны относительно начала удаления; сети под замком нет.
func (g *Gateway) ChangeSwarmOwner(actorID, id, expectedOwnerID, newOwnerID string) error {
	return g.store.WithTx(context.Background(), func(ctx context.Context) error {
		current, err := g.authorizeACLMutation(ctx, actorID, id)
		if err != nil {
			return err
		}
		if current.OwnerID != expectedOwnerID {
			return fmt.Errorf("%w: swarm owner changed", app.ErrConflict)
		}
		if err := g.requireACLGrantee(ctx, current.ProjectID, newOwnerID); err != nil {
			return err
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
	return store.Swarm{
		ID: s.ID, ProjectID: s.ProjectID, OwnerID: s.OwnerID, Name: s.Name,
		AtespaceName: s.AtespaceName, AtespaceUID: nilIfEmpty(s.AtespaceUID),
		State: string(s.State), Retry: s.Retry,
	}
}

// toAppSwarm переводит запись хранилища в роя прикладного слоя.
func toAppSwarm(sw store.Swarm) app.Swarm {
	uid := ""
	if sw.AtespaceUID != nil {
		uid = *sw.AtespaceUID
	}
	result := app.Swarm{
		ID: sw.ID, ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: sw.Name,
		AtespaceName: sw.AtespaceName, AtespaceUID: uid, State: app.SwarmState(sw.State), Retry: sw.Retry,
	}
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
	return app.Swarm{
		ID: sw.ID, ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: sw.Name,
		AtespaceName: sw.AtespaceName, AtespaceUID: sw.AtespaceUID,
		State: app.SwarmState(sw.State), Retry: sw.Retry, CreatedAt: sw.CreatedAt, UpdatedAt: sw.UpdatedAt,
	}
}

// uidChanged сравнивает указатели UID; nil и пустая строка равны.
func uidChanged(a, b *string) bool {
	av, bv := "", ""
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av != bv
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

// ─── Provisioner: atespace через ateapi ────────────────────────────────────

// SubstrateProvisioner — app.Provisioner поверх клиента ateapi: создание,
// чтение и удаление atespace роя. Не настроенный клиент отказывает 503
// с именами настроек.
type SubstrateProvisioner struct {
	// client — nil, когда Substrate не настроен.
	client *substrate.Client
}

// NewSubstrateProvisioner собирает провижинер; nil-клиент допустим.
func NewSubstrateProvisioner(client *substrate.Client) *SubstrateProvisioner {
	return &SubstrateProvisioner{client: client}
}

// CreateAtespace создаёт atespace с данным именем и возвращает его UID.
func (p *SubstrateProvisioner) CreateAtespace(name string) (string, error) {
	client, err := p.configured()
	if err != nil {
		return "", err
	}
	created, err := client.CreateAtespace(context.Background(),
		&ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: name}})
	if err != nil {
		return "", err
	}
	return created.GetMetadata().GetUid(), nil
}

// GetAtespace возвращает UID существующего atespace.
func (p *SubstrateProvisioner) GetAtespace(name string) (string, error) {
	client, err := p.configured()
	if err != nil {
		return "", err
	}
	found, err := client.GetAtespace(context.Background(), name)
	if err != nil {
		var apiErr *substrate.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			return "", fmt.Errorf("%w: atespace not found", app.ErrNotFound)
		}
		return "", err
	}
	return found.GetMetadata().GetUid(), nil
}

// DeleteAtespace удаляет atespace по имени и UID: сохранённый UID —
// предусловие, чужая одноимённая область не удаляется.
func (p *SubstrateProvisioner) DeleteAtespace(name, uid string) error {
	client, err := p.configured()
	if err != nil {
		return err
	}
	current, err := client.GetAtespace(context.Background(), name)
	if err != nil {
		return err
	}
	if uid == "" || current.GetMetadata().GetUid() != uid {
		return fmt.Errorf("%w: atespace %s belongs to uid %s",
			app.ErrConflict, name, current.GetMetadata().GetUid())
	}
	return client.DeleteAtespace(context.Background(), name, uid)
}

// configured возвращает клиент или отказ 503 с именами настроек.
func (p *SubstrateProvisioner) configured() (*substrate.Client, error) {
	if p.client == nil {
		return nil, &notConfiguredError{detail: errAteapiNotConfiguredText}
	}
	return p.client, nil
}

// notConfiguredError — отказ адаптера без настроек: HTTP-слой даёт 503.
type notConfiguredError struct{ detail string }

func (e *notConfiguredError) Error() string { return e.detail }

// notConfigured сообщает, что отказ вызван отсутствием настроек адаптера.
func notConfigured(err error) bool {
	var nc *notConfiguredError
	return errors.As(err, &nc)
}
