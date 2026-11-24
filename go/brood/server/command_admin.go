package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

func adminCommandResult(text string, data any) (command.Result, error) {
	encoded, err := json.Marshal(data)
	return command.Result{Text: text, Data: encoded, Outcome: "completed"}, err
}

// executeAdminCommand executes the commands of a human that are not bound to
// one agent; transport adapters must not substitute guest identities.
func (s *Server) executeAdminCommand(ctx context.Context, userID, swarmID string, inv command.Invocation) (command.Result, error) {
	user, err := s.store.GetUser(ctx, userID)
	if err != nil || user.Disabled {
		return command.Result{}, command.Refuse(app.ErrNotAllowed, "sign in again: the account is not active")
	}
	if inv.Name != "diagnose" && !user.IsAdmin {
		return command.Result{}, command.Refuse(app.ErrNotAllowed, "administrator access required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch inv.Name {
	case "diagnose":
		diagnostics := map[string]any{"mode": "operator", "user": user.Username, "admin": user.IsAdmin}
		if swarmID != "" {
			diagnostics["swarm_id"] = swarmID
		}
		return adminCommandResult("You reach the server as a signed-in user.", diagnostics)
	case "pools":
		return s.commandPools(ctx)
	case "pool_drain", "pool_enable":
		var args struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(inv.Arguments, &args) != nil || args.Name == "" {
			return command.Result{}, command.Refuse(nil, "name the pool to change")
		}
		return s.commandPoolState(ctx, userID, args.Name, inv.Name == "pool_enable")
	case "server_diagnose":
		database := "available"
		if _, err := s.store.CountActiveAdmins(ctx); err != nil {
			database = "unavailable"
		}
		return adminCommandResult("Checks of the running server only; nothing was changed or repaired.", map[string]any{"database": database, "agent_relay_configured": s.provider != nil && s.provider.hub != nil})
	}
	return command.Result{}, command.Refuse(nil, "unknown command")
}

func (s *Server) commandPoolState(ctx context.Context, userID, name string, enable bool) (command.Result, error) {
	if s.provider == nil {
		return command.Result{}, command.Refuse(app.ErrConflict, "Proxmox pools are not configured on this server; nothing changed")
	}
	s.provider.admission.Lock()
	defer s.provider.admission.Unlock()
	pool, err := s.provider.poolByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return command.Result{}, command.Refusef(app.ErrNotFound, "there is no pool named %q; list the pools with the pools command", name)
	}
	if err != nil {
		return command.Result{}, errors.New("the list of pools is unavailable; nothing changed, retry")
	}
	if enable && !s.provider.poolAvailable(pool) {
		return command.Result{}, command.Refusef(app.ErrConflict, "pool %q was not enabled: its Proxmox connection is missing or its settings changed; save the pool settings again in the panel", name)
	}
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		user, err := s.store.GetUser(ctx, userID)
		if err != nil {
			return err
		}
		if user.Disabled || !user.IsAdmin {
			return command.Refuse(app.ErrNotAllowed, "administrator access required")
		}
		pool.State = "draining"
		if enable {
			pool.State = "enabled"
		}
		pool, err = s.store.UpdatePool(ctx, pool, pool.Revision)
		return err
	})
	var refusal *command.Refusal
	switch {
	case errors.As(err, &refusal):
		return command.Result{}, err
	case errors.Is(err, store.ErrRevisionConflict):
		return command.Result{}, command.Refusef(app.ErrConflict, "pool %q changed while the command ran; nothing changed, retry", name)
	case err != nil:
		return command.Result{}, errors.New("the pool state was not saved; nothing changed, retry")
	}
	text := "The pool takes no new agents; agents already in it keep running."
	if enable {
		text = "The pool takes new agents again."
	}
	return adminCommandResult(text, map[string]any{"name": pool.Name, "state": pool.State})
}

func (s *Server) commandPools(ctx context.Context) (command.Result, error) {
	pools, err := s.store.ListPools(ctx)
	if err != nil {
		return command.Result{}, errors.New("the list of pools is unavailable; retry")
	}
	items := []map[string]any{}
	for _, p := range pools {
		if p.State != "deleted" {
			items = append(items, map[string]any{"name": p.Name, "state": p.State, "max_agents": p.MaxActors})
		}
	}
	return adminCommandResult("Proxmox pools.", map[string]any{"pools": items})
}
