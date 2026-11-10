package store

// FEATURE: Ёмкость пула и привязки ролей перед запуском роя (#221).
// SPEC: До запуска рой доказывает, что может занять каждую роль графа:
// роль привязана, актор привязки в фазе running и должен работать,
// принадлежит рою и его atespace в записанной инкарнации, пул актора enabled,
// а различных акторов графа в пуле не больше max_actors. Один актор занимает
// несколько ролей явной привязкой. Отказ называет первую роль по имени и пул,
// акторов проверка не создаёт.
// IMPLEMENTATION: RolesFit — чистая функция над SwarmRoleSnapshot;
// CheckSwarmRolesFit читает снимок в транзакции вызывающего (WithTx) и только
// читает. Пригодность актора — liveRoleActor, общий с BindSwarmRole и
// ResolveSwarmRole, плюс требование фазы running (#211).
// RESULT: IMPLEMENTED — запуск роя (#211) зовёт CheckSwarmRolesFit внутри
// своей транзакции до первой записи; отдельный вызов даёт только совет.
// STATUS: DONE

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"brood.dev/go/brood/pipeline"
)

// SwarmRoleSnapshot is the state RolesFit decides on: the swarm, its stored
// role bindings, the bound actors by UID and their pools by UID. An actor or
// pool absent from its map is treated as missing.
type SwarmRoleSnapshot struct {
	Swarm    Swarm
	Bindings []SwarmRoleBinding
	Actors   map[string]ManagedActor
	Pools    map[string]ManagedPool
}

// RoleFitError names the role and, when the refusal concerns a pool, the
// pool that keeps the swarm from starting. Err wraps the sentinel:
// ErrSwarmRoleUnbound, ErrSwarmRoleInactive, ErrSwarmRoleForeignActor,
// ErrSwarmRoleStale, ErrPoolUnavailable or ErrPoolCapacity.
type RoleFitError struct {
	Role string
	Pool string
	Err  error
}

func (e *RoleFitError) Error() string {
	where := fmt.Sprintf("role %q", e.Role)
	if e.Pool != "" {
		where += fmt.Sprintf(", pool %q", e.Pool)
	}
	return where + ": " + e.Err.Error()
}

func (e *RoleFitError) Unwrap() error { return e.Err }

// RolesFit reports whether the swarm can take every role of the workflow
// document now. Each role must be bound to an actor of the swarm and its
// current atespace at the stored incarnation, the actor must be running and
// meant to run, its pool enabled, and the distinct actors the graph needs in
// a pool must not exceed the pool's max_actors. One actor may hold several
// roles. Roles are checked in name order and the first problem is returned
// as a *RoleFitError; bindings of roles outside the graph are ignored.
func RolesFit(doc *pipeline.Document, snap SwarmRoleSnapshot) error {
	if doc == nil {
		return errors.New("workflow document is required to check swarm roles")
	}
	bound := map[string]SwarmRoleBinding{}
	for _, b := range snap.Bindings {
		if b.SwarmID == snap.Swarm.ID {
			bound[b.Role] = b
		}
	}
	// used — различные акторы графа по UID пула.
	used := map[string]map[string]bool{}
	for _, role := range sortedRoles(doc) {
		b, ok := bound[role]
		if !ok {
			return &RoleFitError{Role: role, Err: ErrSwarmRoleUnbound}
		}
		actor, err := boundRoleActor(snap, b)
		if err != nil {
			return &RoleFitError{Role: role, Err: err}
		}
		pool, ok := snap.Pools[actor.PoolUID]
		if !ok {
			return &RoleFitError{Role: role, Pool: actor.PoolUID, Err: fmt.Errorf("%w: pool does not exist", ErrPoolUnavailable)}
		}
		if pool.State != "enabled" {
			return &RoleFitError{Role: role, Pool: pool.Name, Err: fmt.Errorf("%w: pool is %s", ErrPoolUnavailable, pool.State)}
		}
		if used[pool.UID] == nil {
			used[pool.UID] = map[string]bool{}
		}
		used[pool.UID][actor.UID] = true
		if need := int64(len(used[pool.UID])); need > pool.MaxActors {
			return &RoleFitError{Role: role, Pool: pool.Name, Err: fmt.Errorf("%w: workflow needs %d distinct actors, pool max_actors %d", ErrPoolCapacity, need, pool.MaxActors)}
		}
	}
	return nil
}

// boundRoleActor — актор привязки из снимка и вердикт запуска по нему, без
// пула: общий для RolesFit и представления ролей (ListSwarmRoles, #235).
// Отсутствующий актор — ErrSwarmRoleUnbound и пустой ManagedActor.
func boundRoleActor(snap SwarmRoleSnapshot, b SwarmRoleBinding) (ManagedActor, error) {
	actor, ok := snap.Actors[b.ActorUID]
	if !ok {
		return ManagedActor{}, fmt.Errorf("%w: bound actor %q does not exist", ErrSwarmRoleUnbound, b.ActorUID)
	}
	return actor, runningRoleActor(snap.Swarm, actor, b)
}

// runningRoleActor — пригодность актора под роль при запуске: правило
// liveRoleActor, строже по фазе (provisioning ещё не работает, #211) и с
// инкарнацией привязки, как в ResolveSwarmRole.
func runningRoleActor(swarm Swarm, a ManagedActor, b SwarmRoleBinding) error {
	if err := liveRoleActor(swarm, a); err != nil {
		return err
	}
	if a.Phase != "running" {
		return fmt.Errorf("%w: actor is %s, swarm start requires running", ErrSwarmRoleInactive, a.Phase)
	}
	if a.Incarnation != b.ActorIncarnation {
		return fmt.Errorf("%w: binding records incarnation %d, actor is at %d", ErrSwarmRoleStale, b.ActorIncarnation, a.Incarnation)
	}
	return nil
}

// sortedRoles — роли графа по имени: отказ воспроизводим.
func sortedRoles(doc *pipeline.Document) []string {
	set := doc.Roles()
	roles := make([]string, 0, len(set))
	for role := range set {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// CheckSwarmRolesFit reads the swarm, its role bindings, the bound actors and
// their pools in one transaction and applies RolesFit to the document. It
// writes nothing. Inside the caller's WithTx (swarm start, #211) the verdict
// holds for the caller's own writes; called alone it is advice only, because
// actors and pools can change right after it returns.
func (s *Store) CheckSwarmRolesFit(ctx context.Context, swarmID string, doc *pipeline.Document) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		snap, err := s.swarmRoleSnapshot(ctx, swarmID)
		if err != nil {
			return err
		}
		return RolesFit(doc, snap)
	})
}

// swarmRoleSnapshot читает рой, все его привязки, их акторов по UID (не по
// рою: актор чужого роя должен попасть в снимок и получить свой отказ) и их
// пулы. Отсутствующий актор или пул в снимок не попадает.
func (s *Store) swarmRoleSnapshot(ctx context.Context, swarmID string) (SwarmRoleSnapshot, error) {
	snap := SwarmRoleSnapshot{Actors: map[string]ManagedActor{}, Pools: map[string]ManagedPool{}}
	var err error
	if snap.Swarm, err = s.GetSwarm(ctx, swarmID); err != nil {
		return snap, err
	}
	if snap.Bindings, err = s.SwarmsRoleBindings(ctx, swarmID); err != nil {
		return snap, err
	}
	for _, b := range snap.Bindings {
		if _, seen := snap.Actors[b.ActorUID]; seen {
			continue
		}
		actor, err := s.GetManagedActor(ctx, b.ActorUID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return snap, err
		}
		snap.Actors[actor.UID] = actor
		if _, seen := snap.Pools[actor.PoolUID]; seen {
			continue
		}
		pool, err := s.GetManagedPool(ctx, actor.PoolUID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return snap, err
		}
		snap.Pools[pool.UID] = pool
	}
	return snap, nil
}
