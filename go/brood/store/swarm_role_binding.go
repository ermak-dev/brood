package store

// FEATURE: Привязка роли роя к существующему актору (#208).
// SPEC: swarm_role_bindings(swarm_id, role, actor_uid, actor_incarnation):
// один актор может занимать несколько ролей одного роя; актор чужого роя
// недопустим; устаревшая инкарнация отвергается при использовании, при
// записи сверяется с текущей managed_actors.incarnation.
// IMPLEMENTATION: Таблица в реестре схемы #205, BindSwarmRole пишет привязку
// только живому актору своего активного роя, SwarmsRoleBindings читает,
// ResolveSwarmRole проверяет рой, принадлежность, UID, инкарнацию и
// активность актора одним снимком.
// requireRoleActor — единственный вход для записи актора в шаг: роль узла
// разрешается в транзакции записи, UID и инкарнация шага обязаны совпасть
// с привязкой.
// Управляющий роем (#235) меняет привязки через BindSwarmRoleAs и
// UnbindSwarmRoleAs: сессия, рой, право управлять, исполнение и актор
// проверяются в транзакции записи; при идущем проходе (running, waiting)
// привязки не меняются, иначе шаг прохода адресовал бы другого актора, чем
// тот, с кем проход начался. ListSwarmRoles читает роли графа и привязки с
// вердиктом запуска одним снимком. BindSwarmRole остаётся примитивом без
// проверки прав и исполнения: им заполняют состояние тесты, боевой путь
// идёт через BindSwarmRoleAs.
// RESULT: IMPLEMENTED — привязки ограничены своим роем, живым актором и
// его инкарнацией. Проверка сама ничего не авторизует: побочный эффект
// обязан звать её внутри собственной транзакции (WithTx), иначе между
// проверкой и записью актор может смениться; requireRoleActor вне
// транзакции отказывает. Смена привязок управляющим (#235) схему не меняет.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// ErrSwarmRoleStale indicates that a stored binding names an actor incarnation
// that is no longer current; rebind the role before using it.
var ErrSwarmRoleStale = errors.New("swarm role binding is stale")

// ErrSwarmRoleForeignActor indicates that the actor belongs to another swarm
// or to an atespace that is no longer its swarm's.
var ErrSwarmRoleForeignActor = errors.New("swarm role binding requires an actor of the same swarm")

// ErrSwarmRoleInactive indicates that the swarm or the actor is not live:
// the swarm is not active, the actor is stopped, failed, deleting or deleted,
// or it is no longer meant to run.
var ErrSwarmRoleInactive = errors.New("swarm role binding names an inactive swarm or actor")

// ErrSwarmRoleUnbound indicates that the node's role has no binding in the
// swarm, so no actor may take the step.
var ErrSwarmRoleUnbound = errors.New("node role is not bound to an actor of the swarm")

// ErrSwarmRoleMismatch indicates that a step names an actor or incarnation
// other than the one bound to the node's role.
var ErrSwarmRoleMismatch = errors.New("step actor is not the actor bound to the node's role")

// ErrSwarmRoleInvalid indicates that the role is not a lowercase DNS label.
var ErrSwarmRoleInvalid = errors.New("swarm role must be a lowercase DNS label")

// ErrSwarmRoleBusy indicates that the swarm execution is running or waiting:
// role bindings change only between passes.
var ErrSwarmRoleBusy = errors.New("swarm role bindings do not change while the execution is running or waiting")

// patternSwarmRole — роль та же DNS label, что и имя узла-актора.
var patternSwarmRole = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// swarmRoleBindingSchema — привязки ролей роя к акторам. Регистрация
// в общей схеме — schemaModules ("swarm-roles") и verifySwarmRoleSchema
// в registry.go (#205).
const swarmRoleBindingSchema = `CREATE TABLE swarm_role_bindings (
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    actor_uid TEXT NOT NULL,
    actor_incarnation INTEGER NOT NULL CHECK (actor_incarnation > 0),
    PRIMARY KEY (swarm_id, role)
)`

// SwarmRoleBinding is one role of a swarm resolved to an actor incarnation.
type SwarmRoleBinding struct {
	SwarmID          string
	Role             string
	ActorUID         string
	ActorIncarnation int64
}

// liveRoleActor — единое правило пригодности актора под роль: рой активен,
// актор принадлежит рою и его нынешнему atespace (как в
// authorizeCommandActorDurable), живёт в фазе provisioning или running и
// должен работать. Фазы stopped, stopping, error, deleting и deleted, как и
// желание deleted или stopped, роль не принимают.
func liveRoleActor(swarm Swarm, a ManagedActor) error {
	if a.SwarmID != swarm.ID || swarm.AtespaceUID == nil || *swarm.AtespaceUID != a.AtespaceUID {
		return ErrSwarmRoleForeignActor
	}
	if swarm.State != "active" {
		return fmt.Errorf("%w: swarm is %s", ErrSwarmRoleInactive, swarm.State)
	}
	if a.Desired != "running" || (a.Phase != "provisioning" && a.Phase != "running") {
		return fmt.Errorf("%w: actor is %s, desired %s", ErrSwarmRoleInactive, a.Phase, a.Desired)
	}
	return nil
}

// BindSwarmRole atomically binds a role to a live actor of the same active
// swarm. One actor may hold several roles; rebinding a role moves it to the
// new actor. The stored incarnation is the actor's current one at write
// time; a positive b.ActorIncarnation is the incarnation the caller expects,
// and another current one is refused with ErrSwarmRoleStale. Deleted,
// deleting, failed or stopped actors are refused with ErrSwarmRoleInactive,
// unknown actors and actors of another swarm with ErrSwarmRoleForeignActor,
// a role that is not a DNS label with ErrSwarmRoleInvalid.
//
// BindSwarmRole checks neither the caller's rights nor the execution: a
// manager binds through BindSwarmRoleAs.
func (s *Store) BindSwarmRole(ctx context.Context, b SwarmRoleBinding) error {
	if !patternSwarmRole.MatchString(b.Role) {
		return ErrSwarmRoleInvalid
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		swarm, err := s.GetSwarm(ctx, b.SwarmID)
		if err != nil {
			return err
		}
		actor, err := s.GetManagedActor(ctx, b.ActorUID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: actor %q does not exist", ErrSwarmRoleForeignActor, b.ActorUID)
		}
		if err != nil {
			return err
		}
		if err := liveRoleActor(swarm, actor); err != nil {
			return err
		}
		if b.ActorIncarnation > 0 && b.ActorIncarnation != actor.Incarnation {
			return fmt.Errorf("%w: expected incarnation %d, actor is at %d", ErrSwarmRoleStale, b.ActorIncarnation, actor.Incarnation)
		}
		_, err = s.conn(ctx).ExecContext(ctx,
			`INSERT INTO swarm_role_bindings(swarm_id,role,actor_uid,actor_incarnation) VALUES (?,?,?,?)
			 ON CONFLICT(swarm_id,role) DO UPDATE SET actor_uid=excluded.actor_uid, actor_incarnation=excluded.actor_incarnation`,
			b.SwarmID, b.Role, b.ActorUID, actor.Incarnation)
		return err
	})
}

// SwarmRoleAssignment is a manager's request to bind a role of a swarm to
// an actor of that swarm.
type SwarmRoleAssignment struct {
	SwarmID, UserID, Role, ActorUID string
	// ActorIncarnation, when positive, is the incarnation the caller saw;
	// another current incarnation refuses the binding with ErrSwarmRoleStale.
	ActorIncarnation int64
}

// SwarmRoleGraph names the graph whose roles a role view lists: the pinned
// snapshot of the swarm or, before the first start, a catalog definition.
type SwarmRoleGraph struct {
	Pinned   bool
	Name     string
	Version  int64
	Revision string
	// Roles are the roles of the graph's actor nodes in name order.
	Roles []string
}

// SwarmRoleState is one role of a role view: a role of the graph, a stored
// binding, or both.
type SwarmRoleState struct {
	Role string
	// InGraph reports that the role is a role of the view's graph.
	InGraph bool
	// Binding is the stored binding; nil when the role is not bound.
	Binding *SwarmRoleBinding
	// ActorName is the bound actor's name while it is an actor of the swarm.
	ActorName string
	// Err is the verdict a swarm start would give the role by its actor:
	// nil when the actor can take it, otherwise ErrSwarmRoleUnbound (no
	// binding or the actor is gone), ErrSwarmRoleForeignActor,
	// ErrSwarmRoleInactive or ErrSwarmRoleStale. Pools are not judged here.
	Err error
}

// SwarmRoles is the role view of a swarm: its graph, if any, and every role
// of the graph or of a stored binding, in name order.
type SwarmRoles struct {
	Swarm Swarm
	Graph *SwarmRoleGraph
	Roles []SwarmRoleState
}

// BindSwarmRoleAs binds a role on behalf of a manager and returns the role
// view after the write. One transaction checks the caller's session
// (authorize gets the transaction context), that the swarm is active and the
// user still manages it, that the execution is neither running nor waiting
// (ErrSwarmRoleBusy), and the actor as BindSwarmRole does, before it writes.
// Any refusal writes nothing.
func (s *Store) BindSwarmRoleAs(ctx context.Context, in SwarmRoleAssignment, authorize func(context.Context) bool) (SwarmRoles, error) {
	if !patternSwarmRole.MatchString(in.Role) {
		return SwarmRoles{}, ErrSwarmRoleInvalid
	}
	var view SwarmRoles
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.changeableSwarmRoles(ctx, in.SwarmID, in.UserID, authorize); err != nil {
			return err
		}
		if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: in.SwarmID, Role: in.Role, ActorUID: in.ActorUID, ActorIncarnation: in.ActorIncarnation}); err != nil {
			return err
		}
		var err error
		view, err = s.ListSwarmRoles(ctx, in.SwarmID, "")
		return err
	})
	return view, err
}

// UnbindSwarmRoleAs removes the binding of a role on behalf of a manager
// under the checks of BindSwarmRoleAs and returns the role view after the
// write. A role without a binding is refused with ErrSwarmRoleUnbound.
func (s *Store) UnbindSwarmRoleAs(ctx context.Context, swarmID, userID, role string, authorize func(context.Context) bool) (SwarmRoles, error) {
	if !patternSwarmRole.MatchString(role) {
		return SwarmRoles{}, ErrSwarmRoleInvalid
	}
	var view SwarmRoles
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.changeableSwarmRoles(ctx, swarmID, userID, authorize); err != nil {
			return err
		}
		result, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM swarm_role_bindings WHERE swarm_id=? AND role=?`, swarmID, role)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: role %q", ErrSwarmRoleUnbound, role)
		}
		view, err = s.ListSwarmRoles(ctx, swarmID, "")
		return err
	})
	return view, err
}

// changeableSwarmRoles — общий вход смены привязок: сессия, рой active и
// право управлять им — как у запуска (manageableSwarm), затем исполнение.
// Проход прочитал бы привязку при каждом шаге, поэтому между проходами
// привязки меняются, а во время прохода — нет.
func (s *Store) changeableSwarmRoles(ctx context.Context, swarmID, userID string, authorize func(context.Context) bool) error {
	w, err := s.manageableSwarm(ctx, swarmID, userID, "role binding", authorize)
	if err != nil {
		return err
	}
	if activeExecution(w.Execution) {
		return fmt.Errorf("%w: execution is %s", ErrSwarmRoleBusy, w.Execution)
	}
	return nil
}

// ListSwarmRoles reads the role view of a swarm in one transaction. The
// graph is the pinned snapshot; before the first start it is the catalog
// definition named by definition, and none when definition is empty. A
// pinned swarm refuses another definition with ErrSwarmGraphPinned; a
// definition absent from the swarm's project catalog is
// ErrWorkflowDefinitionMissing, one the runner would refuse is
// ErrWorkflowGraphInvalid. The view writes nothing and authorizes nothing.
func (s *Store) ListSwarmRoles(ctx context.Context, swarmID, definition string) (SwarmRoles, error) {
	var view SwarmRoles
	err := s.WithTx(ctx, func(ctx context.Context) error {
		snap, err := s.swarmRoleSnapshot(ctx, swarmID)
		if err != nil {
			return err
		}
		graph, err := s.swarmRoleGraph(ctx, snap.Swarm, definition)
		if err != nil {
			return err
		}
		view = SwarmRoles{Swarm: snap.Swarm, Graph: graph, Roles: swarmRoleStates(snap, graph)}
		return nil
	})
	return view, err
}

// swarmRoleGraph — граф представления ролей: снимок роя, иначе названное
// определение каталога его проекта, проверенное валидатором исполнителя,
// как при запуске.
func (s *Store) swarmRoleGraph(ctx context.Context, w Swarm, definition string) (*SwarmRoleGraph, error) {
	var graph *SwarmRoleGraph
	var source []byte
	switch {
	case w.Graph != nil:
		if definition != "" && definition != w.DefinitionName {
			return nil, fmt.Errorf("%w: the swarm runs workflow %q", ErrSwarmGraphPinned, w.DefinitionName)
		}
		graph = &SwarmRoleGraph{Pinned: true, Name: w.DefinitionName, Version: w.DefinitionVersion, Revision: w.DefinitionRevision}
		source = w.Graph
	case definition != "":
		def, err := s.GetWorkflowDefinition(ctx, w.ProjectID, definition)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrWorkflowDefinitionMissing, definition)
		}
		if err != nil {
			return nil, err
		}
		graph = &SwarmRoleGraph{Name: def.Name, Version: def.Version, Revision: def.Revision}
		source = []byte(def.YAML)
	default:
		return nil, nil
	}
	doc, err := executableGraph(source)
	if err != nil {
		return nil, fmt.Errorf("%w: workflow %q: %v", ErrWorkflowGraphInvalid, graph.Name, err)
	}
	graph.Roles = sortedRoles(doc)
	return graph, nil
}

// swarmRoleStates сводит роли графа и привязки роя в одну упорядоченную
// по имени роли таблицу с вердиктом запуска по актору (boundRoleActor).
func swarmRoleStates(snap SwarmRoleSnapshot, graph *SwarmRoleGraph) []SwarmRoleState {
	byRole := map[string]*SwarmRoleState{}
	if graph != nil {
		for _, role := range graph.Roles {
			byRole[role] = &SwarmRoleState{Role: role, InGraph: true, Err: ErrSwarmRoleUnbound}
		}
	}
	for _, b := range snap.Bindings {
		state := byRole[b.Role]
		if state == nil {
			state = &SwarmRoleState{Role: b.Role}
			byRole[b.Role] = state
		}
		binding := b
		state.Binding = &binding
		actor, err := boundRoleActor(snap, b)
		state.Err = err
		// Имя актора чужого роя не раскрывается: оно не принадлежит этому рою.
		if actor.UID != "" && actor.SwarmID == snap.Swarm.ID {
			state.ActorName = actor.Name
		}
	}
	states := make([]SwarmRoleState, 0, len(byRole))
	for _, state := range byRole {
		states = append(states, *state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Role < states[j].Role })
	return states
}

// SwarmsRoleBindings lists the role bindings of a swarm ordered by role.
// The list is stored data: it does not say the bindings are still live.
func (s *Store) SwarmsRoleBindings(ctx context.Context, swarmID string) ([]SwarmRoleBinding, error) {
	rows, err := s.conn(ctx).QueryContext(ctx,
		`SELECT swarm_id,role,actor_uid,actor_incarnation FROM swarm_role_bindings WHERE swarm_id=? ORDER BY role`, swarmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SwarmRoleBinding{}
	for rows.Next() {
		var b SwarmRoleBinding
		if err := rows.Scan(&b.SwarmID, &b.Role, &b.ActorUID, &b.ActorIncarnation); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// ResolveSwarmRole returns the binding of a role only while the swarm is
// active and the bound actor is a live actor of that swarm at the stored
// incarnation; otherwise it fails with ErrNotFound (no binding),
// ErrSwarmRoleStale, ErrSwarmRoleForeignActor or ErrSwarmRoleInactive.
//
// The three reads happen in one transaction, so the verdict is a single
// snapshot. It is still only a verdict: called outside the transaction of
// the side effect it authorizes nothing, because the actor can change
// right after it returns. A dispatching effect must call it inside the same
// WithTx that records the effect.
func (s *Store) ResolveSwarmRole(ctx context.Context, swarmID, role string) (SwarmRoleBinding, error) {
	var b SwarmRoleBinding
	err := s.WithTx(ctx, func(ctx context.Context) error {
		err := s.conn(ctx).QueryRowContext(ctx,
			`SELECT swarm_id,role,actor_uid,actor_incarnation FROM swarm_role_bindings WHERE swarm_id=? AND role=?`, swarmID, role).
			Scan(&b.SwarmID, &b.Role, &b.ActorUID, &b.ActorIncarnation)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: swarm role %q", ErrNotFound, role)
		}
		if err != nil {
			return err
		}
		swarm, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		actor, err := s.GetManagedActor(ctx, b.ActorUID)
		if err != nil {
			return err
		}
		if err := liveRoleActor(swarm, actor); err != nil {
			return err
		}
		if actor.Incarnation != b.ActorIncarnation {
			return ErrSwarmRoleStale
		}
		return nil
	})
	if err != nil {
		return SwarmRoleBinding{}, err
	}
	return b, nil
}

// requireRoleActor допускает запись шага только для актора, привязанного к
// роли узла, в его нынешней инкарнации: актор чужого роя, неживой актор,
// устаревшая привязка и любой другой UID или инкарнация — отказ. Проверка
// зовётся внутри транзакции записи шага, иначе между ней и записью актор
// может смениться; вне транзакции она отказывает сама. Отсутствие привязки
// или её актора — ErrSwarmRoleUnbound, не ErrNotFound: отказ шагу — конфликт,
// а не ненайденный ресурс.
func (s *Store) requireRoleActor(ctx context.Context, swarmID, role, actorUID string, incarnation int64) error {
	if _, ok := ctx.Value(txKey{}).(*sql.Tx); !ok {
		return errors.New("swarm role check must run inside the step transaction")
	}
	b, err := s.ResolveSwarmRole(ctx, swarmID, role)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: role %q: %v", ErrSwarmRoleUnbound, role, err)
	}
	if err != nil {
		return err
	}
	if b.ActorUID != actorUID || b.ActorIncarnation != incarnation {
		return ErrSwarmRoleMismatch
	}
	return nil
}
