package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type sessionRelay interface {
	CallSession(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error)
}

// readonlyTerminalOperation is the guest's read-only screen snapshot protocol
// behind the read-only terminal page (serveReadonlyActorView). It is not a
// catalog command.
const readonlyTerminalOperation = "session_terminal"

func sessionCommandTarget(inv command.Invocation, fallback string) (string, bool, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(inv.Arguments, &args); err != nil {
		return "", false, err
	}
	if args.Name == "" {
		args.Name = fallback
	}
	if args.Name == "" {
		return "", false, command.Refuse(nil, "name the agent")
	}
	return args.Name, inv.Name == "slash" || inv.Name == "restart", nil
}

const agentOffline = "the agent is not connected, so nothing was sent; check its state with agents"

// callCommandSession sends one session operation to the agent's guest. A
// mutation holds a workspace reservation: reserve takes it after authorize
// passed, and a restart without reserve takes it first. The reservation is
// released when the guest confirms the outcome and kept when the outcome is
// unknown.
func (m *managedProvider) callCommandSession(ctx context.Context, target store.ManagedActor, caller string, inv command.Invocation, authorize func() error, reserve func(context.Context) (store.ActorOperation, error)) (result command.Result, err error) {
	hub, ok := m.hub.(sessionRelay)
	if !ok {
		return result, command.Refuse(nil, agentOffline)
	}
	confirmed := true
	var held *store.ActorOperation
	defer func() {
		if held == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		finishErr := m.server.store.FinishActorOperation(cleanup, *held, confirmed)
		if finishErr != nil || !confirmed {
			result.Outcome = "unconfirmed"
			err = command.Refusef(nil, "it is unknown whether %s of agent %q completed; look at the agent with tail before retrying", inv.Name, target.Name)
		}
	}()
	if reserve == nil && inv.Name == "restart" {
		op, acquireErr := m.server.store.AcquireActorOperation(ctx, target.UID, target.Incarnation, store.NewID(), caller, inv.Name)
		if acquireErr != nil {
			return result, actorCommandError(acquireErr)
		}
		held = &op
	}
	if !m.hub.Connected(target.UID, target.Incarnation) {
		return result, command.Refuse(nil, agentOffline)
	}
	if authorize != nil {
		if err := authorize(); err != nil {
			return result, err
		}
	}
	if reserve != nil {
		op, acquireErr := reserve(ctx)
		if acquireErr != nil {
			return result, actorCommandError(acquireErr)
		}
		held = &op
	}
	if held != nil && !held.Owner {
		ctx = m.fenceSessionOpen(ctx, *held, nil)
	}
	confirmed = false
	reply, rpcErr := hub.CallSession(ctx, target.UID, target.Incarnation, relay.SessionRequest{Operation: inv.Name, Arguments: inv.Arguments, Caller: caller})
	if rpcErr != nil {
		return command.Result{Outcome: "unconfirmed"}, command.Refusef(nil, "agent %q did not answer; it is unknown whether %s completed, look at the agent with tail before retrying", target.Name, inv.Name)
	}
	confirmed = reply.Outcome != "unknown" && reply.Outcome != "unconfirmed"
	if !reply.OK {
		return command.Result{Outcome: reply.Outcome}, errors.New(reply.Text)
	}
	result = command.Result{Text: reply.Text, Outcome: reply.Outcome}
	if json.Valid([]byte(reply.Text)) {
		result.Data = json.RawMessage(reply.Text)
	}
	return result, nil
}

func (m *managedProvider) executeGuestSessionCommand(ctx context.Context, from store.ManagedActor, inv command.Invocation) (command.Result, error) {
	name, mutate, err := sessionCommandTarget(inv, from.Name)
	if err != nil {
		return command.Result{}, err
	}
	target, err := m.authorizeCommandTarget(ctx, from, name, mutate)
	if err != nil {
		return command.Result{}, actorCommandError(err)
	}
	before := func() error {
		fresh, err := m.authorizeCommandTarget(ctx, from, name, mutate)
		if err != nil || fresh.UID != target.UID || fresh.Incarnation != target.Incarnation {
			return actorCommandError(app.ErrNotAllowed)
		}
		return ctx.Err()
	}
	caller := fmt.Sprintf("actor:%s:%d", from.UID, from.Incarnation)
	var reserve func(context.Context) (store.ActorOperation, error)
	if inv.Name == "slash" {
		reserve = func(ctx context.Context) (store.ActorOperation, error) {
			return m.server.store.AcquireActorSessionOperation(ctx, target, store.NewID(), caller, inv.Name, func(ctx context.Context) error {
				current, _, err := m.authorizeCommandActorDurable(ctx, from)
				if err != nil {
					return err
				}
				user, err := m.server.store.GetUser(ctx, current.DelegatorID)
				if err != nil || user.Disabled || !user.IsAdmin || current.Role != "master" || current.DelegatorID != from.DelegatorID {
					return app.ErrNotAllowed
				}
				if err := m.server.authorizeSwarmManagerIntent(ctx, current.DelegatorID, current.SwarmID); err != nil {
					return err
				}
				return m.masterOwns(ctx, current, target)
			})
		}
	}
	result, err := m.callCommandSession(ctx, target, caller, inv, before, reserve)
	fresh, authErr := m.authorizeCommandTarget(ctx, from, name, mutate)
	if authErr != nil || fresh.UID != target.UID || fresh.Incarnation != target.Incarnation {
		return command.Result{}, actorCommandError(app.ErrNotAllowed)
	}
	return result, err
}

type commandSessionAuthorityKey struct{}

func commandSessionValid(ctx context.Context) bool {
	if valid, ok := ctx.Value(commandSessionAuthorityKey{}).(func(context.Context) bool); ok {
		return valid(ctx)
	}
	return true
}

func (s *Server) operatorSessionAuthority(ctx context.Context, userID, swarmID string, target store.ManagedActor) error {
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil || sw.State != "active" {
		return app.ErrNotAllowed
	}
	allowed, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	if err != nil || !allowed {
		return app.ErrNotAllowed
	}
	if current, err := s.store.GetManagedActor(ctx, target.UID); err != nil || current.Incarnation != target.Incarnation || current.Desired != "running" {
		return app.ErrNotAllowed
	}
	if !commandSessionValid(ctx) {
		return app.ErrNotAllowed
	}
	return ctx.Err()
}

// executeOperatorSessionCommand executes tail, slash and restart of a human,
// and the read-only terminal snapshot of the panel.
func (s *Server) executeOperatorSessionCommand(ctx context.Context, userID, swarmID, caller string, inv command.Invocation) (command.Result, error) {
	allowed, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	if err != nil {
		return command.Result{}, err
	}
	if !allowed {
		return command.Result{}, actorCommandError(app.ErrNotAllowed)
	}
	name, mutate, err := sessionCommandTarget(inv, "")
	if err != nil {
		return command.Result{}, err
	}
	if _, err := s.store.GetSwarm(ctx, swarmID); err != nil {
		return command.Result{}, err
	}
	target, err := s.store.LookupManagedActor(ctx, swarmID, name)
	if err != nil {
		return command.Result{}, actorCommandError(app.ErrNotFound)
	}
	owner := s.authorizeSwarmOwnerIntent(ctx, userID, swarmID) == nil
	before := func() error {
		if err := s.operatorSessionAuthority(ctx, userID, swarmID, target); err != nil {
			return actorCommandError(err)
		}
		return ctx.Err()
	}
	var reserve func(context.Context) (store.ActorOperation, error)
	if inv.Name == "slash" {
		reserve = func(ctx context.Context) (store.ActorOperation, error) {
			return s.store.AcquireActorSessionOperation(ctx, target, store.NewID(), caller, inv.Name, func(ctx context.Context) error {
				return s.authorizeSwarmManagerIntent(ctx, userID, swarmID)
			})
		}
	}
	if mutate && owner {
		reserve = func(ctx context.Context) (store.ActorOperation, error) {
			release, err := s.managed.effects.acquire(ctx, target.UID)
			if err != nil {
				return store.ActorOperation{}, err
			}
			defer release()
			return s.store.AcquireOwnerActorOperation(ctx, target, store.NewID(), caller, inv.Name, func(ctx context.Context) error {
				return s.authorizeSwarmOwnerIntent(ctx, userID, swarmID)
			})
		}
	}
	result, err := s.managed.callCommandSession(ctx, target, caller, inv, before, reserve)
	if authErr := before(); authErr != nil {
		return command.Result{}, authErr
	}
	return result, err
}
