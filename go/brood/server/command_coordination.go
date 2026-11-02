package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// executeCoordinationCommand executes managed-master coordination, not human
// impersonation. Dispatch reserves an allocation, not a verified clean checkout.
// Landing is project-wide advisory coordination, not Git merge enforcement.
func (m *managedProvider) executeCoordinationCommand(ctx context.Context, from store.ManagedActor, inv command.Invocation) (command.Result, error) {
	sw, err := m.authorizeCoordination(ctx, from)
	if err != nil {
		return command.Result{}, err
	}
	var result command.Result
	switch inv.Name {
	case "dispatch":
		result, err = m.executeDispatch(ctx, from, sw, inv.Arguments)
	case "landing":
		result, err = m.executeLanding(ctx, from, sw, inv.Arguments)
	default:
		return command.Result{}, fmt.Errorf("unsupported coordination command %q", inv.Name)
	}
	if err != nil {
		return command.Result{}, err
	}
	if _, err := m.authorizeCoordination(ctx, from); err != nil {
		return command.Result{}, err
	}
	return result, nil
}

func (m *managedProvider) authorizeCoordination(ctx context.Context, from store.ManagedActor) (store.Swarm, error) {
	if err := m.authorizeMaster(ctx, from); err != nil {
		return store.Swarm{}, err
	}
	current, err := m.server.store.GetManagedActor(ctx, from.UID)
	if err != nil {
		return store.Swarm{}, err
	}
	if current.Incarnation != from.Incarnation || current.SwarmID != from.SwarmID || current.AtespaceUID != from.AtespaceUID || current.DelegatorID != from.DelegatorID || current.Name != from.Name {
		return store.Swarm{}, app.ErrNotAllowed
	}
	sw, err := m.server.store.GetSwarm(ctx, current.SwarmID)
	if err != nil {
		return store.Swarm{}, err
	}
	if sw.State != "active" || sw.AtespaceUID == nil || *sw.AtespaceUID != current.AtespaceUID {
		return store.Swarm{}, fmt.Errorf("%w: managed swarm binding changed", app.ErrConflict)
	}
	if err := m.server.checkSwarmNamespace(ctx, toAppSwarm(sw)); err != nil {
		return store.Swarm{}, err
	}
	// Namespace RPC can outlive the human grant; authorize after it as well.
	if err := m.authorizeMaster(ctx, from); err != nil {
		return store.Swarm{}, err
	}
	return sw, nil
}

func coordinationArguments(data json.RawMessage, out any) error {
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return fmt.Errorf("coordination arguments must be an object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid coordination arguments: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("coordination arguments must contain one object")
	}
	return nil
}

func coordinationClaim(from store.ManagedActor, sw store.Swarm, requestID string) store.CoordinationClaim {
	return store.CoordinationClaim{ProjectID: sw.ProjectID, SwarmID: sw.ID, RequestID: requestID, Source: store.CoordinationActor{UID: from.UID, Generation: from.Incarnation}, HumanID: from.DelegatorID}
}

type dispatchCoordinationResult struct {
	RequestID        string `json:"request_id"`
	Target           string `json:"target"`
	TargetUID        string `json:"target_uid"`
	TargetGeneration int64  `json:"target_generation"`
	Delivery         string `json:"delivery"`
	LeaseActive      bool   `json:"lease_active"`
	Workspace        string `json:"workspace"`
}

func coordinationResult(text, outcome string, data any) (command.Result, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return command.Result{}, err
	}
	return command.Result{Text: text, Outcome: outcome, Data: payload}, nil
}

func (m *managedProvider) dispatchTarget(ctx context.Context, from, target store.ManagedActor) error {
	current, err := m.server.store.GetManagedActor(ctx, target.UID)
	if err != nil {
		return err
	}
	if current.Incarnation != target.Incarnation || current.Name != target.Name || current.UID == from.UID || current.SwarmID != from.SwarmID || current.AtespaceUID != from.AtespaceUID || current.Phase != "running" || current.Desired != "running" || current.Operation != "start" || current.Stage != "done" {
		return fmt.Errorf("%w: recipient is not another active managed generation in this swarm", app.ErrConflict)
	}
	return nil
}

func (m *managedProvider) executeDispatch(ctx context.Context, from store.ManagedActor, sw store.Swarm, data json.RawMessage) (command.Result, error) {
	var args struct {
		Action                string `json:"action"`
		To                    string `json:"to"`
		Message               string `json:"message"`
		RequestID             string `json:"request_id"`
		RequireCleanWorkspace bool   `json:"require_clean_workspace"`
	}
	if err := coordinationArguments(data, &args); err != nil {
		return command.Result{}, err
	}
	if args.Action == "" {
		args.Action = "send"
	}
	if strings.TrimSpace(args.To) == "" || strings.TrimSpace(args.RequestID) == "" {
		return command.Result{}, fmt.Errorf("dispatch requires to and request_id")
	}
	if args.Action == "show" || args.Action == "release" {
		return m.dispatchLeaseAction(ctx, from, sw, args.To, args.RequestID, args.Action)
	}
	if args.Action != "send" || strings.TrimSpace(args.Message) == "" {
		return command.Result{}, fmt.Errorf("dispatch send requires message")
	}
	if args.RequireCleanWorkspace {
		return command.Result{}, fmt.Errorf("workspace cleanliness proof is unavailable; dispatch only provides an allocation lease")
	}
	if len(args.Message) > relay.MaxMessageBytes {
		return command.Result{}, fmt.Errorf("dispatch message exceeds size limit")
	}
	target, err := m.server.store.LookupManagedActor(ctx, from.SwarmID, args.To)
	if err != nil {
		return command.Result{}, fmt.Errorf("dispatch recipient is not another active managed actor in this swarm")
	}
	if err := m.dispatchTarget(ctx, from, target); err != nil {
		return command.Result{}, err
	}
	claim := coordinationClaim(from, sw, args.RequestID)
	claim.Target = store.CoordinationActor{UID: target.UID, Generation: target.Incarnation}
	claim.PayloadHash = fmt.Sprintf("%x", sha256.Sum256([]byte(args.Message)))
	fresh, active := false, false
	// Проверка слота и claim в одной транзакции: только первый запрос доставляет.
	// История отпущенного request_id не позволяет вновь занять свободный слот.
	err = m.server.store.WithTx(ctx, func(tx context.Context) error {
		_, e := m.server.store.ShowDispatchLease(tx, sw.ProjectID, sw.ID, target.UID)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return e
		}
		vacant := errors.Is(e, store.ErrNotFound)
		if _, e = m.server.store.ClaimDispatchLease(tx, claim); e != nil {
			return e
		}
		after, e := m.server.store.ShowDispatchLease(tx, sw.ProjectID, sw.ID, target.UID)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return e
		}
		active = e == nil && after == claim
		fresh = vacant && active
		return nil
	})
	if err != nil {
		return command.Result{}, err
	}
	delivery := "not_retried"
	if fresh {
		// Не освобождаем lease при неизвестной доставке или отзыве полномочий.
		delivery = "unconfirmed"
		if m.hub != nil && m.hub.Connected(target.UID, target.Incarnation) && m.deliverMessage != nil {
			if _, err := m.authorizeCoordination(ctx, from); err != nil {
				return command.Result{}, err
			}
			if err := m.dispatchTarget(ctx, from, target); err != nil {
				return command.Result{}, err
			}
			current, e := m.server.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, target.UID)
			if e != nil || current != claim {
				return command.Result{}, fmt.Errorf("%w: dispatch allocation lease is no longer held", app.ErrConflict)
			}
			response, e := m.deliverMessage(ctx, target.UID, target.Incarnation, relay.MessageRequest{To: target.Name, From: from.Name, Body: args.Message})
			if e == nil && response.OK {
				delivery = "confirmed"
			}
		}
	}
	result := dispatchCoordinationResult{RequestID: claim.RequestID, Target: target.Name, TargetUID: target.UID, TargetGeneration: target.Incarnation, Delivery: delivery, LeaseActive: active, Workspace: "unverified"}
	text := fmt.Sprintf("Dispatch request %q for %q (uid %s, generation %d): delivery=%s, lease_active=%t. Allocation lease only; workspace cleanliness is unverified. Retries never resend; an unconfirmed delivery retains its claim.", claim.RequestID, target.Name, target.UID, target.Incarnation, delivery, active)
	return coordinationResult(text, delivery, result)
}

type landingCoordinationResult struct {
	RequestID        string `json:"request_id,omitempty"`
	HolderUID        string `json:"holder_uid,omitempty"`
	HolderGeneration int64  `json:"holder_generation,omitempty"`
	SwarmID          string `json:"swarm_id,omitempty"`
	HumanID          string `json:"human_id,omitempty"`
	Active           bool   `json:"active"`
	Advisory         bool   `json:"advisory"`
}

func (m *managedProvider) executeLanding(ctx context.Context, from store.ManagedActor, sw store.Swarm, data json.RawMessage) (command.Result, error) {
	var args struct {
		Action    string `json:"action"`
		Name      string `json:"name"`
		RequestID string `json:"request_id"`
	}
	if err := coordinationArguments(data, &args); err != nil {
		return command.Result{}, err
	}
	if args.Action == "" {
		args.Action = "show"
	}
	if args.Name != "" && args.Name != from.Name {
		return command.Result{}, fmt.Errorf("landing name must equal the calling managed master; impersonation is not supported")
	}
	if args.Action != "show" && args.Action != "take" && args.Action != "give" {
		return command.Result{}, fmt.Errorf("landing action must be show, take or give")
	}
	if args.Action == "take" && strings.TrimSpace(args.RequestID) == "" {
		return command.Result{}, fmt.Errorf("landing take requires request_id")
	}
	var holder store.CoordinationClaim
	active := false
	err := m.server.store.WithTx(ctx, func(tx context.Context) error {
		var e error
		if args.Action == "take" {
			holder, e = m.server.store.TakeLandingToken(tx, coordinationClaim(from, sw, args.RequestID))
			if e != nil {
				return e
			}
			current, err := m.server.store.ShowLandingToken(tx, sw.ProjectID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			// A released request reports its own inactive claim, not a successor.
			active = err == nil && current == holder
			return nil
		}
		holder, e = m.server.store.ShowLandingToken(tx, sw.ProjectID)
		if errors.Is(e, store.ErrNotFound) {
			if args.Action == "give" {
				return fmt.Errorf("%w: landing token is not held", app.ErrConflict)
			}
			return nil
		}
		if e != nil {
			return e
		}
		active = true
		if args.Action == "give" {
			if holder.Source != (store.CoordinationActor{UID: from.UID, Generation: from.Incarnation}) || holder.HumanID != from.DelegatorID || holder.SwarmID != sw.ID || (args.RequestID != "" && args.RequestID != holder.RequestID) {
				return fmt.Errorf("%w: landing token belongs to another holder or request", app.ErrConflict)
			}
			if e = m.server.store.GiveLandingToken(tx, holder); e != nil {
				return e
			}
			active = false
		}
		return nil
	})
	if err != nil {
		return command.Result{}, err
	}
	result := landingCoordinationResult{RequestID: holder.RequestID, HolderUID: holder.Source.UID, HolderGeneration: holder.Source.Generation, SwarmID: holder.SwarmID, HumanID: holder.HumanID, Active: active, Advisory: true}
	text := fmt.Sprintf("Project-wide advisory landing token: active=%t, request_id=%q, holder_uid=%q, holder_generation=%d, swarm_id=%q. This does not enforce Git merges.", active, holder.RequestID, holder.Source.UID, holder.Source.Generation, holder.SwarmID)
	return coordinationResult(text, "ok", result)
}
