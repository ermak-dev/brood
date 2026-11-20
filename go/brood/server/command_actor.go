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
	switch inv.Name {
	case "add", "delete", "stop", "start", "reset", "restart", "update", "recycle", "wipe":
	default:
		return args, command.Refuse(nil, "unsupported actor lifecycle command")
	}
	parsed, err := command.ParseTool(inv.Name, inv.Arguments)
	if err != nil {
		return args, err
	}
	if err = json.Unmarshal(parsed.Arguments, &args); err != nil {
		return args, err
	}
	if len(args.Name) > 63 || !managedActorName.MatchString(args.Name) {
		return args, command.Refuse(nil, "actor name must be a DNS label of at most 63 characters")
	}
	switch inv.Name {
	case "restart":
		return args, command.Refuse(nil, "restart is unsupported: managed actors have no durable workspace-preserving restart intent; reset destroys the disk and is not a substitute")
	case "recycle", "wipe":
		return args, command.Refuse(nil, fmt.Sprintf("%s is unsupported: managed reset does not implement this command's workspace and session semantics", inv.Name))
	case "delete":
		if !args.Confirm {
			return args, command.Refuse(nil, "delete destroys the actor disk; confirm=true is required")
		}
	case "reset":
		if !args.Confirm {
			return args, command.Refuse(nil, "reset destroys the actor container and workspace; confirm=true is required")
		}
	}
	return args, nil
}

// authorizeCommandActor resolves the current relay identity and namespace binding.
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

// authorizeCommandTarget is shared with session command adapters. Workers may
// read only themselves. Peer targeting requires a currently approved master,
// and a mutation additionally requires that the master added the target
// (masterOwns). Callers must retain the returned UID/generation and recheck
// before side effects.
func (m *managedProvider) authorizeCommandTarget(ctx context.Context, original store.ManagedActor, name string, mutate bool) (store.ManagedActor, error) {
	from, _, err := m.authorizeCommandActor(ctx, original)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if mutate || name != from.Name {
		if err := m.authorizeMaster(ctx, from); err != nil {
			return store.ManagedActor{}, err
		}
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
// changes an actor: lifecycle, workspace and session control (#152), and the
// dispatch lease that holds it (#267). Its refusal is of class
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
	// Master управляет только workers, добавленными им через add, как mop
	// отказывает мастеру в чужом папете. ParentUID неизменен с резервирования,
	// а ревизию цели и делегацию перепроверяет сама операция.
	if from.UID != "" && target.ParentUID == from.UID {
		return ""
	}
	const scope = "a master manages only the workers it added"
	switch {
	case target.UID == from.UID:
		// Себя master не останавливает, не удаляет, не перезапускает и не
		// очищает. В mop master не папет, а stop остаётся командой
		// оператора; диспетчер шагов перезапускает сессии актора сам, без
		// гостевых команд. Остановивший себя master не может ни доложить, ни подняться:
		// start себя ему не доступен, и рой остаётся без координатора до
		// решения оператора.
		return fmt.Sprintf("actor command not authorized: master %q does not manage itself; the swarm operator does", from.Name)
	case target.Role == "master":
		return fmt.Sprintf("actor command not authorized: %q is another master; %s", target.Name, scope)
	case target.ParentUID == "":
		return fmt.Sprintf("actor command not authorized: %q was created by the swarm operator, not added by master %q; %s", target.Name, from.Name, scope)
	case owner != "":
		return fmt.Sprintf("actor command not authorized: %q is a worker of master %q; %s", target.Name, owner, scope)
	default:
		return fmt.Sprintf("actor command not authorized: %q is a worker of another master; %s", target.Name, scope)
	}
}

// executeLifecycleCommand executes only the explicitly supported managed intents.
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
	if err = m.authorizeMaster(ctx, from); err != nil {
		return command.Result{}, actorCommandError(err)
	}
	if inv.Name == "add" {
		pool, err := m.server.store.GetManagedPool(ctx, from.PoolUID)
		if err != nil || pool.SourceFingerprint != from.SourceFingerprint {
			return command.Result{}, command.Refuse(nil, "actor creation refused: approved managed pool binding is unavailable")
		}
		a, err := m.server.createManaged(ctx, from.DelegatorID, sw, args.Name, pool, "worker", args.Origin, &from)
		if err != nil && a.UID == "" {
			return command.Result{}, command.Refuse(nil, managedAddRefusal(err))
		}
		return actorCommandResult(a), nil
	}
	target, err := m.authorizeCommandTarget(ctx, from, args.Name, true)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	return m.server.applyActorCommand(ctx, from.DelegatorID, sw, target, inv.Name, &from)
}

// executeActorCommand executes a human-authorized managed command without HTTP.
// Add requires a delegated approved pool; this name-only operator interface cannot
// guess a pool or template. Native actor lifecycle is not implemented here.
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
	if inv.Name == "add" {
		return command.Result{}, command.Refuse(nil, "operator add is unsupported without an approved delegated pool; use explicit actor creation")
	}
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	a, err := s.store.LookupManagedActor(ctx, swarmID, args.Name)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	return s.applyActorCommand(ctx, userID, toAppSwarm(sw), a, inv.Name, nil)
}

func (s *Server) applyActorCommand(ctx context.Context, userID string, sw app.Swarm, a store.ManagedActor, action string, parent *store.ManagedActor) (command.Result, error) {
	if s.managed == nil {
		return command.Result{}, command.Refuse(nil, "managed lifecycle is unavailable")
	}
	pool, err := s.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	if action == "update" {
		return s.updateActor(ctx, userID, sw, pool, a, parent)
	}
	op := &managedWorkloadOperation{server: s, context: ctx, userID: userID, swarm: sw, pool: pool, name: a.Name, action: action, actor: a, parent: parent}
	err = s.admitWorkload(userID, sw.ID, op)
	if err != nil && op.saved.UID == "" {
		if op.intent.UID != "" {
			result := actorCommandResult(op.intent)
			result.Outcome = "unknown"
			result.Text = "The lifecycle intent may have been accepted, but its provider task was not confirmed; check the actor before retrying."
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
	var held *store.CoordinationConflictError
	var waiting *waitingRefusal
	switch {
	case errors.As(err, &refusal):
		// Владение (#152) и отказы, уже ставшие отказом команды.
		return err
	case errors.As(err, &waiting):
		// Ожидающий update называет ожидание и последний сбой фразой
		// диагностики актора (#133), без текста ошибки провайдера.
		return command.Refuse(app.ErrConflict, waiting.Error())
	case errors.As(err, &held):
		// Причина координации не выходит за рой актора и называет держателя
		// аренды (#272): её отдаём как есть, а не общим отказом.
		return command.Refusef(app.ErrConflict, "managed lifecycle refused; %s", held.Reason)
	case errors.Is(err, store.ErrRevisionConflict):
		// Проход reconcile (например, повтор отказавшего Start update, #133)
		// или другая команда изменили актора между чтением и записью
		// намерения: сверять нечего, повтор прочитает новое состояние.
		return command.Refusef(app.ErrConflict, "managed lifecycle refused; the actor changed while the command was checked (a reconcile pass or another lifecycle change); retry the command")
	case errors.Is(err, app.ErrNotAllowed):
		return command.Refusef(app.ErrNotAllowed, "actor command delegation or swarm access is not authorized")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, app.ErrNotFound):
		return command.Refusef(app.ErrNotFound, "managed actor is not available in this swarm")
	default:
		return command.Refusef(app.ErrConflict, "managed lifecycle refused; reconcile the current allocation and verify namespace and provider availability")
	}
}

func actorCommandResult(a store.ManagedActor) command.Result {
	data, _ := json.Marshal(struct {
		UID         string `json:"uid"`
		Name        string `json:"name"`
		Generation  int64  `json:"generation"`
		OperationID string `json:"operation_id"`
		Operation   string `json:"operation"`
		Stage       string `json:"stage"`
		State       string `json:"state"`
		Desired     string `json:"desired"`
	}{a.UID, a.Name, a.Incarnation, a.OperationID, a.Operation, a.Stage, a.Phase, a.Desired})
	return command.Result{Outcome: "accepted", Text: fmt.Sprintf("actor %q intent accepted (uid %s); provisioning and cleanup are asynchronous, not a completion or readiness confirmation; current state: %s", a.Name, a.UID, a.Phase), Data: data}
}
