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

func sessionCommandTarget(inv command.Invocation, fallback string) (string, bool, error) {
	var args struct {
		Name   string `json:"name"`
		Action string `json:"action"`
		Mode   string `json:"mode"`
	}
	if err := json.Unmarshal(inv.Arguments, &args); err != nil {
		return "", false, err
	}
	if args.Name == "" {
		args.Name = fallback
	}
	if args.Name == "" {
		return "", false, command.Refuse(nil, "select a target actor with --name")
	}
	mutate := inv.Name == "slash" || inv.Name == "wipe" || inv.Name == "recycle" || inv.Name == "restart"
	if inv.Name == "session_terminal" {
		mutate = args.Action == "input" || args.Action == "interrupt" || args.Action == "resize" || (args.Action == "open" && args.Mode == "rw")
	}
	return args.Name, mutate, nil
}

func (m *managedProvider) callCommandSession(ctx context.Context, target store.ManagedActor, caller string, inv command.Invocation, authorize func() error) (result command.Result, err error) {
	hub, ok := m.hub.(sessionRelay)
	if !ok {
		return result, command.Refuse(nil, "recipient session relay is unavailable")
	}
	confirmed := true
	if inv.Name == "restart" || inv.Name == "wipe" || inv.Name == "recycle" {
		op, acquireErr := m.server.store.AcquireActorOperation(ctx, target.UID, target.Incarnation, store.NewID(), caller, inv.Name)
		if acquireErr != nil {
			return result, command.Refusef(app.ErrConflict, "actor workspace operation is held or conflicts with coordination")
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finishErr := m.server.store.FinishActorOperation(cleanup, op, confirmed)
			if finishErr != nil || !confirmed {
				result.Outcome = "unconfirmed"
				err = fmt.Errorf("workspace operation %s outcome is unconfirmed; reservation retained; inspect before explicit recovery or retry", op.OperationID)
			}
		}()
	}
	if !m.hub.Connected(target.UID, target.Incarnation) {
		return result, command.Refuse(nil, "recipient session relay is unavailable")
	}
	if authorize != nil {
		if err := authorize(); err != nil {
			return result, err
		}
	}
	confirmed = false
	reply, rpcErr := hub.CallSession(ctx, target.UID, target.Incarnation, relay.SessionRequest{Operation: inv.Name, Arguments: inv.Arguments, Caller: caller})
	if rpcErr != nil {
		return command.Result{Outcome: "unconfirmed"}, errors.New("session operation outcome is unconfirmed; check before retrying")
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

func (s *Server) authorizeSessionCoordination(ctx context.Context, target store.ManagedActor, sourceUID string, generation int64, userID, operation string) error {
	claims, err := s.store.ActorCoordinationClaims(ctx, target.UID)
	if err != nil {
		return app.ErrConflict
	}
	for _, claim := range claims {
		reason := "release active coordination before restarting or cleaning this actor"
		if operation != "restart" && operation != "wipe" && operation != "recycle" {
			if (sourceUID != "" && claim.Source.UID == sourceUID && claim.Source.Generation == generation) || (sourceUID == "" && userID == claim.HumanID) {
				continue
			}
			reason = "session control belongs to another dispatch owner"
		}
		// Отказ называет держателя и request ID, по которому аренду
		// освобождают (#272).
		hold, err := s.store.DescribeCoordinationHold(ctx, target.SwarmID, claim)
		if err != nil {
			return app.ErrConflict
		}
		return command.Refusef(app.ErrConflict, "%s: %s", reason, hold)
	}
	return nil
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
	if mutate {
		if err := m.server.authorizeSessionCoordination(ctx, target, from.UID, from.Incarnation, from.DelegatorID, inv.Name); err != nil {
			return command.Result{}, err
		}
	}
	before := func() error {
		fresh, err := m.authorizeCommandTarget(ctx, from, name, mutate)
		if err != nil || fresh.UID != target.UID || fresh.Incarnation != target.Incarnation {
			return app.ErrNotAllowed
		}
		if mutate {
			if err := m.server.authorizeSessionCoordination(ctx, target, from.UID, from.Incarnation, from.DelegatorID, inv.Name); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	result, err := m.callCommandSession(ctx, target, fmt.Sprintf("actor:%s:%d", from.UID, from.Incarnation), inv, before)
	fresh, authErr := m.authorizeCommandTarget(ctx, from, name, mutate)
	if authErr != nil || fresh.UID != target.UID || fresh.Incarnation != target.Incarnation {
		return command.Result{}, app.ErrNotAllowed
	}
	if err != nil {
		return result, err
	}
	return result, nil
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
	if err != nil || sw.State != "active" || sw.AtespaceUID == nil || *sw.AtespaceUID != target.AtespaceUID {
		return app.ErrNotAllowed
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(sw)); err != nil {
		return err
	}
	allowed, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	if err != nil || !allowed {
		return app.ErrNotAllowed
	}
	if current, err := s.store.GetManagedActor(ctx, target.UID); err != nil || current.Incarnation != target.Incarnation || current.Desired != "running" || current.AtespaceUID != target.AtespaceUID {
		return app.ErrNotAllowed
	}
	if !commandSessionValid(ctx) {
		return app.ErrNotAllowed
	}
	return ctx.Err()
}

func (s *Server) executeOperatorSessionCommand(ctx context.Context, userID, swarmID, caller string, inv command.Invocation) (command.Result, error) {
	allowed, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	if err != nil {
		return command.Result{}, err
	}
	if !allowed {
		return command.Result{}, app.ErrNotAllowed
	}
	name, mutate, err := sessionCommandTarget(inv, "")
	if err != nil {
		return command.Result{}, err
	}
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil {
		return command.Result{}, err
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(sw)); err != nil {
		return command.Result{}, err
	}
	target, err := s.store.LookupManagedActor(ctx, swarmID, name)
	if err != nil || sw.AtespaceUID == nil || target.AtespaceUID != *sw.AtespaceUID {
		return command.Result{}, app.ErrNotFound
	}
	if mutate {
		if err := s.authorizeSessionCoordination(ctx, target, "", 0, userID, inv.Name); err != nil {
			return command.Result{}, err
		}
	}
	before := func() error {
		if err := s.operatorSessionAuthority(ctx, userID, swarmID, target); err != nil {
			return err
		}
		if mutate {
			if err := s.authorizeSessionCoordination(ctx, target, "", 0, userID, inv.Name); err != nil {
				return err
			}
		}
		if !commandSessionValid(ctx) {
			return app.ErrNotAllowed
		}
		return ctx.Err()
	}
	result, err := s.managed.callCommandSession(ctx, target, caller, inv, before)
	if authErr := before(); authErr != nil {
		return command.Result{}, authErr
	}
	return result, err
}
