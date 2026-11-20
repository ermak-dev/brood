package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
)

func adminCommandResult(text string, data any) (command.Result, error) {
	encoded, err := json.Marshal(data)
	return command.Result{Text: text, Data: encoded, Outcome: "ok"}, err
}

func adminCommandArgs(raw json.RawMessage, out any) error {
	if len(raw) > 1<<20 || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return fmt.Errorf("arguments must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid command arguments")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("invalid command arguments")
	}
	return nil
}

// executeAdminCommand executes human commands; transport adapters must not substitute guest identities.
func (s *Server) executeAdminCommand(ctx context.Context, userID, swarmID string, inv command.Invocation) (command.Result, error) {
	user, err := s.store.GetUser(ctx, userID)
	if err != nil || user.Disabled {
		return command.Result{}, fmt.Errorf("active authenticated user required")
	}
	switch inv.Name {
	case "models", "profile":
	case "project_add", "project_limit", "node", "node_drain", "node_up", "server_diagnose":
		if !user.IsAdmin {
			return command.Result{}, fmt.Errorf("administrator access required")
		}
	default:
		return command.Result{}, fmt.Errorf("unknown administration command")
	}
	validated, err := command.ParseTool(inv.Name, inv.Arguments)
	if err != nil {
		return command.Result{}, err
	}
	inv = validated
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch inv.Name {
	case "project_add":
		var args struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := adminCommandArgs(inv.Arguments, &args); err != nil {
			return command.Result{}, err
		}
		if strings.TrimSpace(args.Name) == "" {
			return command.Result{}, fmt.Errorf("project name is required")
		}
		if swarmID != "" {
			return command.Result{}, fmt.Errorf("project creation requires global operator scope")
		}
		p, err := s.store.CreateAdminProject(ctx, userID, args.Name, args.Description)
		if err != nil {
			return command.Result{}, err
		}
		return adminCommandResult("Brood project created; no source checkout or build was performed.", map[string]any{"id": p.ID, "name": p.Name, "description": args.Description})
	case "project_limit":
		return s.commandProjectLimit(ctx, userID, swarmID, inv.Arguments)
	case "node":
		if err := adminCommandArgs(inv.Arguments, &struct{}{}); err != nil {
			return command.Result{}, err
		}
		return s.commandNodeInventory(ctx)
	case "node_drain", "node_up":
		var args struct {
			Name string `json:"name"`
		}
		if err := adminCommandArgs(inv.Arguments, &args); err != nil {
			return command.Result{}, err
		}
		if swarmID != "" {
			return command.Result{}, fmt.Errorf("node changes require global operator scope")
		}
		return s.commandNodeState(ctx, userID, args.Name, inv.Name == "node_up")
	case "models":
		if err := adminCommandArgs(inv.Arguments, &struct{}{}); err != nil {
			return command.Result{}, err
		}
		return s.adminModels.models(ctx)
	case "profile":
		if err := adminCommandArgs(inv.Arguments, &struct{}{}); err != nil {
			return command.Result{}, err
		}
		profile := map[string]any{"username": user.Username, "user_id": user.ID, "admin": user.IsAdmin, "scope": "global", "capabilities": []string{"models", "profile"}}
		if swarmID != "" {
			sw, err := s.app.GetSwarm(userID, swarmID)
			if err != nil {
				return command.Result{}, err
			}
			profile["scope"] = "swarm"
			profile["swarm_id"] = sw.ID
			profile["project_id"] = sw.ProjectID
		}
		if user.IsAdmin {
			profile["capabilities"] = []string{"models", "profile", "project_limit", "node", "server_diagnose"}
			if swarmID == "" {
				profile["capabilities"] = []string{"models", "profile", "project_add", "project_limit", "node", "node_drain", "node_up", "server_diagnose"}
			}
		}
		return adminCommandResult("Authenticated Brood operator identity and command scope; not a model subscription profile.", profile)
	case "server_diagnose":
		if err := adminCommandArgs(inv.Arguments, &struct{}{}); err != nil {
			return command.Result{}, err
		}
		_, dbErr := s.store.CountActiveAdmins(ctx)
		database := "available"
		if dbErr != nil {
			database = "unavailable"
		}
		return adminCommandResult("Remote server runtime checks only; no local database migration or repair was performed.", map[string]any{"database": database, "managed_relay_configured": s.managed != nil && s.managed.hub != nil, "models_configured": s.adminModels != nil})
	}
	return command.Result{}, fmt.Errorf("unknown administration command")
}

// SetProjectActorLimit adapts the narrow atomic store operation to app admission locking.
func (g *Gateway) SetProjectActorLimit(actorID, projectID string, actors *int64) error {
	return g.store.SetProjectActorLimit(context.Background(), actorID, projectID, actors)
}

func (s *Server) commandProjectLimit(ctx context.Context, userID, swarmID string, raw json.RawMessage) (command.Result, error) {
	var args struct {
		Name   string `json:"name"`
		Actors *int64 `json:"actors"`
	}
	if err := adminCommandArgs(raw, &args); err != nil {
		return command.Result{}, err
	}
	if args.Actors != nil && *args.Actors < 0 {
		return command.Result{}, fmt.Errorf("actors must be nonnegative")
	}
	var projectID string
	if swarmID != "" {
		sw, err := s.app.GetSwarm(userID, swarmID)
		if err != nil {
			return command.Result{}, err
		}
		p, err := s.store.GetProject(ctx, sw.ProjectID)
		if err != nil {
			return command.Result{}, err
		}
		if args.Name != "" && args.Name != p.Name {
			return command.Result{}, fmt.Errorf("project is outside command scope")
		}
		projectID = p.ID
	} else {
		if args.Name == "" {
			return command.Result{}, fmt.Errorf("project name is required outside swarm scope")
		}
		p, ok, err := s.catalog.ProjectByName(ctx, args.Name)
		if err != nil {
			return command.Result{}, err
		}
		if !ok {
			return command.Result{}, app.ErrNotFound
		}
		projectID = p.ID
	}
	if args.Actors != nil {
		if err := s.app.SetShardActorLimit(userID, projectID, args.Actors); err != nil {
			return command.Result{}, err
		}
	}
	c, err := s.store.GetProjectCapacity(ctx, projectID)
	if err != nil {
		return command.Result{}, err
	}
	return adminCommandResult("Brood project actor admission limit; existing actors are not stopped or evicted.", map[string]any{"project_id": projectID, "actors": c.MaxActors, "unlimited": c.MaxActors == nil})
}

func adminNodeName(name string) (string, string, error) {
	provider, id, ok := strings.Cut(name, ":")
	if !ok || (provider != "managed" && provider != "native") || id == "" || len(id) > 128 {
		return "", "", fmt.Errorf("name must be provider-qualified: managed:POOL or native:WORKER")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return "", "", fmt.Errorf("invalid provider-qualified node name")
		}
	}
	if id == "." || id == ".." {
		return "", "", fmt.Errorf("invalid provider-qualified node name")
	}
	return provider, id, nil
}
func (s *Server) commandNodeState(ctx context.Context, userID, name string, up bool) (command.Result, error) {
	provider, id, err := adminNodeName(name)
	if err != nil {
		return command.Result{}, err
	}
	if provider != "managed" {
		return command.Result{}, fmt.Errorf("only managed Proxmox pools are supported")
	}
	if s.managed == nil {
		return command.Result{}, fmt.Errorf("managed pool service unavailable")
	}
	s.managed.admission.Lock()
	defer s.managed.admission.Unlock()
	pool, err := s.managed.poolByName(ctx, id)
	if err != nil {
		return command.Result{}, err
	}
	if up && !s.managed.poolAvailable(pool) {
		return command.Result{}, fmt.Errorf("installed managed pool configuration is unavailable or changed")
	}
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		user, err := s.store.GetUser(ctx, userID)
		if err != nil {
			return err
		}
		if user.Disabled || !user.IsAdmin {
			return fmt.Errorf("administrator access required")
		}
		pool.State = "draining"
		if up {
			pool.State = "enabled"
		}
		pool, err = s.store.UpdateManagedPool(ctx, pool, pool.Revision)
		return err
	})
	if err != nil {
		return command.Result{}, err
	}
	return adminCommandResult("Managed pool admission changed only; existing actors are not stopped, evacuated or migrated.", map[string]any{"name": name, "state": pool.State, "evacuation_confirmed": false})
}

func (s *Server) commandNodeInventory(ctx context.Context) (command.Result, error) {
	items := []map[string]any{}
	available := map[string]string{"managed_pools": "available"}
	pools, err := s.store.ListManagedPools(ctx)
	if err != nil {
		return command.Result{}, errors.New("managed pool inventory unavailable")
	}
	for _, p := range pools {
		if p.State != "deleted" {
			items = append(items, map[string]any{"name": "managed:" + p.Name, "provider": "managed", "kind": "Pool", "state": p.State, "max_actors": p.MaxActors})
		}
	}
	return adminCommandResult("Managed Proxmox pool inventory.", map[string]any{"items": items, "sources": available})
}
