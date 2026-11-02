package server

import (
	"context"
	"encoding/json"
	"errors"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

func (s *Server) executeOperatorCoordinationCommand(ctx context.Context, userID, swarmID string, inv command.Invocation) (command.Result, error) {
	user, err := s.store.GetUser(ctx, userID)
	if err != nil || user.Disabled || !user.IsAdmin {
		return command.Result{}, app.ErrNotAllowed
	}
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil || sw.State != "active" {
		return command.Result{}, app.ErrNotFound
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(sw)); err != nil {
		return command.Result{}, err
	}
	var args struct {
		To        string `json:"to"`
		Name      string `json:"name"`
		Action    string `json:"action"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(inv.Arguments, &args) != nil {
		return command.Result{}, errors.New("invalid coordination arguments")
	}
	action := args.Action
	if action == "" {
		if inv.Name == "dispatch" {
			action = "send"
		} else {
			action = "show"
		}
	}
	if inv.Name == "dispatch" && action != "show" && action != "release" {
		return command.Result{}, errors.New("operator dispatch supports show/release only; sending tasks requires a managed master identity")
	}
	if inv.Name == "landing" && action != "show" && action != "give" {
		return command.Result{}, errors.New("operator landing supports show/give only; taking a token requires a managed master identity")
	}
	var claim store.CoordinationClaim
	var workspace *store.ActorOperation
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		if !commandSessionValid(ctx) {
			return app.ErrNotAllowed
		}
		current, err := s.store.GetUser(ctx, userID)
		if err != nil || current.Disabled || !current.IsAdmin {
			return app.ErrNotAllowed
		}
		if inv.Name == "dispatch" {
			target, err := s.store.LookupManagedActor(ctx, swarmID, args.To)
			if err != nil {
				return app.ErrNotFound
			}
			claim, err = s.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, target.UID)
			if errors.Is(err, store.ErrNotFound) {
				op, opErr := s.store.CurrentActorOperation(ctx, target.UID)
				if opErr != nil {
					return opErr
				}
				workspace = &op
				if action == "show" {
					return nil
				}
				if args.RequestID != op.OperationID || op.State != "unconfirmed" {
					return errors.New("workspace recovery requires the exact unconfirmed operation ID; inflight operations cannot be released")
				}
				return s.store.FinishActorOperation(ctx, op, true)
			}
			if err != nil {
				return err
			}
		} else {
			claim, err = s.store.ShowLandingToken(ctx, sw.ProjectID)
			if err != nil {
				return err
			}
		}
		if action == "show" {
			return nil
		}
		if args.RequestID == "" || args.RequestID != claim.RequestID {
			return errors.New("recovery requires the exact active request_id")
		}
		if inv.Name == "dispatch" {
			return s.store.ReleaseDispatchLease(ctx, claim)
		}
		return s.store.GiveLandingToken(ctx, claim)
	})
	if err != nil {
		return command.Result{}, err
	}
	if workspace != nil {
		return coordinationResult("Explicit workspace reservation "+action+"; inspect the workspace before retrying any side effect.", "completed", map[string]any{"operation_id": workspace.OperationID, "operation": workspace.Operation, "state": workspace.State, "active": action == "show"})
	}
	return coordinationResult("Explicit operator coordination "+action+"; no actor identity was impersonated and no task was sent.", "completed", map[string]any{"request_id": claim.RequestID, "source_uid": claim.Source.UID, "target_uid": claim.Target.UID, "active": action == "show"})
}
