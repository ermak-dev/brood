package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

type actorCommandArguments struct {
	Name    string `json:"name"`
	Confirm bool   `json:"confirm"`
	Origin  string `json:"origin"`
}

func parseActorCommand(inv command.Invocation) (actorCommandArguments, error) {
	var args actorCommandArguments
	parsed, err := command.ParseTool(inv.Name, inv.Arguments)
	if err != nil {
		return args, err
	}
	if err = json.Unmarshal(parsed.Arguments, &args); err != nil {
		return args, err
	}
	if len(args.Name) > 63 || !managedActorName.MatchString(args.Name) {
		return args, command.Refuse(nil, "an agent name is lowercase letters, digits and dashes, at most 63 characters; nothing changed")
	}
	switch inv.Name {
	case "rm":
		if !args.Confirm {
			return args, command.Refuse(nil, "rm deletes the agent with its container and workspace; nothing changed, repeat with confirm=true")
		}
	case "reset":
		if !args.Confirm {
			return args, command.Refuse(nil, "reset destroys the agent's container and workspace; nothing changed, repeat with confirm=true")
		}
	}
	return args, nil
}

// authorizeCommandActor resolves the current relay identity and its swarm.
// Only UID, generation and binding from the supplied snapshot are used as identity;
// role and delegation always come from the durable row.
func (m *managedProvider) authorizeCommandActor(ctx context.Context, original store.ManagedActor) (store.ManagedActor, app.Swarm, error) {
	a, swarm, err := m.authorizeCommandActorDurable(ctx, original)
	if err != nil {
		return store.ManagedActor{}, app.Swarm{}, err
	}
	return a, swarm, nil
}

func (m *managedProvider) authorizeCommandActorDurable(ctx context.Context, original store.ManagedActor) (store.ManagedActor, app.Swarm, error) {
	a, err := m.server.store.GetManagedActor(ctx, original.UID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ManagedActor{}, app.Swarm{}, app.ErrNotAllowed
		}
		return store.ManagedActor{}, app.Swarm{}, err
	}
	if a.Incarnation != original.Incarnation || a.SwarmID != original.SwarmID || a.Phase != "running" || a.Desired != "running" || a.Operation != "start" || a.Stage != "done" {
		return store.ManagedActor{}, app.Swarm{}, app.ErrNotAllowed
	}
	sw, err := m.server.store.GetSwarm(ctx, a.SwarmID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ManagedActor{}, app.Swarm{}, app.ErrNotAllowed
		}
		return store.ManagedActor{}, app.Swarm{}, err
	}
	if sw.State != "active" {
		return store.ManagedActor{}, app.Swarm{}, app.ErrNotAllowed
	}
	return a, toAppSwarm(sw), nil
}

// authorizeCommandTarget is shared with session command adapters. An agent may
// read only itself. Peer targeting requires a currently approved master,
// and a mutation additionally requires that the master added the target
// (masterOwns). Callers must retain the returned UID/generation and recheck
// before side effects.
func (m *managedProvider) authorizeCommandTarget(ctx context.Context, original store.ManagedActor, name, tool string, mutate bool) (store.ManagedActor, error) {
	from, _, err := m.authorizeCommandActor(ctx, original)
	if err != nil {
		return store.ManagedActor{}, err
	}
	permission := tool
	if tool == "tail" {
		permission = "tail:self"
		if name != from.Name {
			permission = "tail:swarm"
		}
	}
	if err := m.authorizeGuestPermission(ctx, from, permission); err != nil {
		return store.ManagedActor{}, err
	}
	target, err := m.server.store.LookupManagedActor(ctx, from.SwarmID, name)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if target.Phase == "deleted" {
		return store.ManagedActor{}, app.ErrNotAllowed
	}
	if mutate {
		if err := m.masterOwns(ctx, from, target); err != nil {
			return store.ManagedActor{}, err
		}
	}
	return target, nil
}

// masterOwns is the single ownership check behind every guest command that
// changes an agent: lifecycle, workspace and session control (#152). Its refusal is of class
// app.ErrNotAllowed, and its text names whose actor the target is.
func (m *managedProvider) masterOwns(ctx context.Context, from, target store.ManagedActor) error {
	owner := ""
	if target.ParentUID != "" && target.ParentUID != from.UID {
		// Имя владельца подсказывает, кого спросить; без имени отказ
		// остаётся отказом.
		if parent, err := m.server.store.GetManagedActor(ctx, target.ParentUID); err == nil && parent.SwarmID == from.SwarmID {
			owner = parent.Name
		}
	}
	if reason := ownershipRefusal(from, target, owner); reason != "" {
		return command.Refuse(app.ErrNotAllowed, reason)
	}
	return nil
}

// ownershipRefusal returns why master from may not change target, or "" when
// from added target through add. owner names the master that added target, if
// known.
func ownershipRefusal(from, target store.ManagedActor, owner string) string {
	// Master управляет только агентами, добавленными им через add.
	// ParentUID неизменен с резервирования, а ревизию цели и делегацию
	// перепроверяет сама операция.
	if from.UID != "" && target.ParentUID == from.UID {
		return ""
	}
	const scope = "a master manages only the agents it added; nothing changed"
	switch {
	case target.UID == from.UID:
		// Себя master не останавливает, не удаляет и не перезапускает.
		// Остановивший себя master не может ни доложить, ни подняться:
		// start себя ему не доступен, и рой остаётся без координатора до
		// решения оператора.
		return fmt.Sprintf("master %q does not manage itself; the operator does; nothing changed", from.Name)
	case target.ParentUID == "":
		return fmt.Sprintf("%q was created by the operator, not added by master %q; %s", target.Name, from.Name, scope)
	case owner != "":
		return fmt.Sprintf("%q was added by master %q; %s", target.Name, owner, scope)
	default:
		return fmt.Sprintf("%q was added by another master; %s", target.Name, scope)
	}
}

// executeLifecycleCommand executes add, rm, start, stop and reset of a master.
// Acceptance refers to a durable reservation or operation, never guest readiness.
func (m *managedProvider) executeLifecycleCommand(ctx context.Context, original store.ManagedActor, inv command.Invocation) (command.Result, error) {
	args, err := parseActorCommand(inv)
	if err != nil {
		return command.Result{}, err
	}
	from, sw, err := m.authorizeCommandActor(ctx, original)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	if err = m.authorizeGuestPermission(ctx, from, inv.Name); err != nil {
		return command.Result{}, actorCommandError(err)
	}
	if inv.Name == "add" {
		pool, err := m.server.store.GetManagedPool(ctx, from.PoolUID)
		if err != nil {
			return command.Result{}, command.Refuse(nil, "The agent was not created: the pool of this master is no longer available. Ask the operator.")
		}
		a, err := m.server.createManaged(ctx, from.DelegatorID, sw, args.Name, pool, "", args.Origin, &from)
		if err != nil && a.UID == "" {
			return command.Result{}, command.Refuse(nil, managedAddRefusal(err))
		}
		return actorCommandResult(a), nil
	}
	target, err := m.authorizeCommandTarget(ctx, from, args.Name, inv.Name, true)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	return m.server.applyActorCommand(ctx, from.DelegatorID, sw, target, lifecycleAction(inv.Name), &from)
}

// executeActorCommand executes a lifecycle command of a human. Add creates the
// agent in the pool of the swarm's master: the command names no pool.
func (s *Server) executeActorCommand(ctx context.Context, userID, swarmID string, inv command.Invocation) (command.Result, error) {
	args, err := parseActorCommand(inv)
	if err != nil {
		return command.Result{}, err
	}
	allowed, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	if !allowed {
		return command.Result{}, app.ErrNotAllowed
	}
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	if inv.Name == "add" {
		return command.Result{}, command.Refuse(nil, "The agent was not created: this command does not choose a pool. Create the agent in the panel, or let the master of the swarm add it.")
	}
	a, err := s.store.LookupManagedActor(ctx, swarmID, args.Name)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	return s.applyActorCommand(ctx, userID, toAppSwarm(sw), a, lifecycleAction(inv.Name), nil)
}

// lifecycleAction maps a catalog command to the lifecycle operation it starts.
func lifecycleAction(name string) string {
	if name == "rm" {
		return "delete"
	}
	return name
}

func (s *Server) applyActorCommand(ctx context.Context, userID string, sw app.Swarm, a store.ManagedActor, action string, parent *store.ManagedActor) (command.Result, error) {
	if s.managed == nil {
		return command.Result{}, command.Refuse(nil, "Proxmox pools are not configured on this server; nothing changed")
	}
	pool, err := s.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	op := &managedWorkloadOperation{server: s, context: ctx, userID: userID, swarm: sw, pool: pool, name: a.Name, action: action, actor: a, parent: parent}
	err = s.admitWorkload(userID, sw.ID, op)
	if err != nil && op.saved.UID == "" {
		if op.intent.UID != "" {
			result := actorCommandResult(op.intent)
			result.Outcome = "unknown"
			result.Text = "The command may have been accepted, but Proxmox did not confirm it. Check the agent with agents before retrying."
			return result, errors.New(result.Text)
		}
		return command.Result{}, actorCommandError(err)
	}
	return actorCommandResult(op.saved), nil
}

// actorCommandError делает ошибку жизненного цикла отказом команды
// (command.Refusal). Причину называют только сознательные отказы; любая
// другая ошибка, в том числе сбой хранилища или провайдера, становится общим
// отказом без её текста.
func actorCommandError(err error) error {
	var refusal *command.Refusal
	var held *store.OperationConflictError
	var waiting *waitingRefusal
	switch {
	case errors.As(err, &refusal):
		// Владение (#152) и отказы, уже ставшие отказом команды.
		return err
	case errors.As(err, &waiting):
		// Ожидание называет себя и последний сбой фразой диагностики
		// агента, без текста ошибки провайдера.
		return command.Refuse(app.ErrConflict, waiting.Error())
	case errors.As(err, &held):
		return command.Refusef(app.ErrConflict, "nothing changed: %s; check the agent and retry", held.Reason)
	case errors.Is(err, store.ErrRevisionConflict):
		// Проход reconcile или другая команда изменили агента между чтением
		// и записью намерения: повтор прочитает новое состояние.
		return command.Refusef(app.ErrConflict, "the agent changed while the command was checked; nothing changed, retry the command")
	case errors.Is(err, app.ErrNotAllowed):
		return command.Refusef(app.ErrNotAllowed, "you are not allowed to manage agents of this swarm; nothing changed")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, app.ErrNotFound):
		return command.Refusef(app.ErrNotFound, "there is no such agent in this swarm; list the agents with agents")
	default:
		return command.Refusef(app.ErrConflict, "the command was not accepted: the server or Proxmox is unavailable; check the agent with agents and retry")
	}
}

func actorCommandResult(a store.ManagedActor) command.Result {
	data, _ := json.Marshal(struct {
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
		State      string `json:"state"`
		Desired    string `json:"desired"`
	}{a.Name, a.Incarnation, a.Phase, a.Desired})
	return command.Result{Outcome: "accepted", Text: fmt.Sprintf("Accepted for agent %q; it is carried out in the background, so the agent is not ready yet. Current state: %s. Check with agents.", a.Name, a.Phase), Data: data}
}
