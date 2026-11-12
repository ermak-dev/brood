package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
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
		return args, fmt.Errorf("unsupported actor lifecycle command")
	}
	parsed, err := command.ParseTool(inv.Name, inv.Arguments)
	if err != nil {
		return args, err
	}
	if err = json.Unmarshal(parsed.Arguments, &args); err != nil {
		return args, err
	}
	if len(args.Name) > 63 || !managedActorName.MatchString(args.Name) {
		return args, fmt.Errorf("actor name must be a DNS label of at most 63 characters")
	}
	switch inv.Name {
	case "restart":
		return args, fmt.Errorf("restart is unsupported: managed actors have no durable workspace-preserving restart intent; reset destroys the disk and is not a substitute")
	case "update":
		return args, fmt.Errorf("update is unsupported: managed approved profiles are immutable; no in-place profile reconciliation capability is available")
	case "recycle", "wipe":
		return args, fmt.Errorf("%s is unsupported: managed reset does not implement this command's workspace and session semantics", inv.Name)
	case "delete":
		if !args.Confirm {
			return args, fmt.Errorf("delete destroys the actor disk; confirm=true is required")
		}
	case "reset":
		if !args.Confirm {
			return args, fmt.Errorf("reset destroys the actor container and workspace; confirm=true is required")
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
	if err := m.server.checkSwarmNamespace(ctx, swarm); err != nil {
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
	if a.Incarnation != original.Incarnation || a.SwarmID != original.SwarmID || a.AtespaceUID != original.AtespaceUID || a.Phase != "running" || a.Desired != "running" || a.Operation != "start" || a.Stage != "done" {
		return store.ManagedActor{}, app.Swarm{}, app.ErrNotAllowed
	}
	sw, err := m.server.store.GetSwarm(ctx, a.SwarmID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ManagedActor{}, app.Swarm{}, app.ErrNotAllowed
		}
		return store.ManagedActor{}, app.Swarm{}, err
	}
	if sw.State != "active" || sw.AtespaceUID == nil || *sw.AtespaceUID != a.AtespaceUID {
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
	if target.AtespaceUID != from.AtespaceUID || target.Phase == "deleted" {
		return store.ManagedActor{}, app.ErrNotAllowed
	}
	if mutate {
		if err := m.masterOwns(ctx, from, target); err != nil {
			return store.ManagedActor{}, err
		}
	}
	return target, nil
}

// ownershipError is a master's refusal for an actor it did not add. It unwraps
// to app.ErrNotAllowed; its text names whose actor the target is.
type ownershipError struct{ reason string }

func (e *ownershipError) Error() string { return e.reason }
func (e *ownershipError) Unwrap() error { return app.ErrNotAllowed }

// masterOwns is the single ownership check behind every guest command that
// changes an actor: lifecycle, workspace and session control (#152), and the
// dispatch lease that holds it (#267).
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
		return &ownershipError{reason: reason}
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
		if err != nil || pool.Template != from.Template || pool.SourceFingerprint != from.SourceFingerprint {
			return command.Result{}, fmt.Errorf("actor creation refused: approved managed pool binding is unavailable")
		}
		input := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Name: args.Name}, ActorTemplate: &ateapi.ObjectRef{Name: managedTemplatePrefix + pool.Name}}
		a, err := m.server.createManaged(ctx, from.DelegatorID, sw, input, pool, "worker", args.Origin, &from)
		if err != nil && a.UID == "" {
			return command.Result{}, fmt.Errorf("%s", managedAddRefusal(err))
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
		return command.Result{}, fmt.Errorf("operator add is unsupported without an approved delegated pool; use explicit actor creation")
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
		return command.Result{}, fmt.Errorf("managed lifecycle is unavailable")
	}
	if a.AtespaceUID != sw.AtespaceUID {
		return command.Result{}, fmt.Errorf("%w: managed actor namespace binding changed", app.ErrConflict)
	}
	if s.ateapi == nil {
		return command.Result{}, fmt.Errorf("%w: native inventory unavailable", app.ErrConflict)
	}
	_, nativeErr := s.ateapi.GetActor(ctx, sw.AtespaceName, a.Name)
	var api *substrate.APIError
	if !errors.As(nativeErr, &api) || api.Status != 404 {
		return command.Result{}, fmt.Errorf("%w: native actor collision or inventory unavailable", app.ErrConflict)
	}
	pool, err := s.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	op := &managedWorkloadOperation{server: s, context: ctx, userID: userID, swarm: sw, pool: pool, name: a.Name, action: action, actor: a, parent: parent}
	err = s.admitWorkload(userID, sw.ID, op)
	if err != nil && op.saved.UID == "" {
		return command.Result{}, actorCommandError(err)
	}
	return actorCommandResult(op.saved), nil
}

func actorCommandError(err error) error {
	var ownership *ownershipError
	var held *store.CoordinationConflictError
	switch {
	case errors.As(err, &ownership):
		return err
	case errors.As(err, &held):
		// Причина координации не выходит за рой актора и называет держателя
		// аренды (#272): её отдаём как есть, а не общим отказом.
		return fmt.Errorf("%w: managed lifecycle refused; %s", app.ErrConflict, held.Reason)
	case errors.Is(err, app.ErrNotAllowed):
		return fmt.Errorf("%w: actor command delegation or swarm access is not authorized", app.ErrNotAllowed)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, app.ErrNotFound):
		return fmt.Errorf("%w: managed actor is not available in this swarm", app.ErrNotFound)
	default:
		return fmt.Errorf("%w: managed lifecycle refused; reconcile the current allocation and verify namespace and provider availability", app.ErrConflict)
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
