package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"brood.dev/go/brood/app"
)

type capacityJSON struct {
	MaxSwarms        *string `json:"max_swarms"`
	MaxActors        *string `json:"max_actors"`
	MaxCPUMillicores *string `json:"max_cpu_millicores"`
	MaxMemoryBytes   *string `json:"max_memory_bytes"`
}

type usageJSON struct {
	Swarms        string `json:"swarms"`
	Actors        string `json:"actors"`
	CPUMillicores string `json:"cpu_millicores"`
	MemoryBytes   string `json:"memory_bytes"`
}

var capacityDecimal = regexp.MustCompile(`^(?:0|[1-9][0-9]*)$`)

func limitString(value *int64) *string {
	if value == nil {
		return nil
	}
	result := strconv.FormatInt(*value, 10)
	return &result
}

func formatCapacity(capacity app.ShardCapacity) capacityJSON {
	return capacityJSON{MaxSwarms: limitString(capacity.MaxSwarms), MaxActors: limitString(capacity.MaxActors), MaxCPUMillicores: limitString(capacity.MaxCPUMillicores), MaxMemoryBytes: limitString(capacity.MaxMemoryBytes)}
}

func parseCapacity(body capacityJSON) (app.ShardCapacity, error) {
	result := app.ShardCapacity{}
	for _, field := range []struct {
		name   string
		input  *string
		output **int64
	}{
		{"max_swarms", body.MaxSwarms, &result.MaxSwarms}, {"max_actors", body.MaxActors, &result.MaxActors},
		{"max_cpu_millicores", body.MaxCPUMillicores, &result.MaxCPUMillicores}, {"max_memory_bytes", body.MaxMemoryBytes, &result.MaxMemoryBytes},
	} {
		if field.input == nil {
			continue
		}
		if !capacityDecimal.MatchString(*field.input) {
			return result, fmt.Errorf("%w: %s must be a nonnegative decimal string within int64", app.ErrInvalidCapacity, field.name)
		}
		value, err := strconv.ParseInt(*field.input, 10, 64)
		if err != nil {
			return result, fmt.Errorf("%w: %s must be a nonnegative decimal string within int64", app.ErrInvalidCapacity, field.name)
		}
		*field.output = &value
	}
	return result, nil
}

func (s *Server) handleShardCapacity(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return
	}
	var body capacityJSON
	if !decodeAccessBody(w, r, &body) {
		return
	}
	capacity, err := parseCapacity(body)
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.app.SetShardCapacity(user.ID, r.PathValue("id"), capacity); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	capacity, err := s.app.ShardCapacity(user.ID, id)
	if errors.Is(err, app.ErrNotAllowed) {
		err = app.ErrNotFound
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		apiError(w, r, err)
		return
	}
	swarms, err := s.gateway.SwarmsByProject(id)
	if err != nil {
		apiError(w, r, err)
		return
	}
	var usage *usageJSON
	unavailable := ""
	live := false
	for _, swarm := range swarms {
		if swarm.State != app.StateDeleted {
			live = true
			break
		}
	}
	var reservations []app.ActorReservation
	if live {
		reservations, err = s.shardReservations(r.Context(), swarms)
	}
	if err == nil {
		var used app.ShardUsage
		used, err = app.ShardUsageFromRecords(id, swarms, reservations)
		if err == nil {
			usage = &usageJSON{Swarms: strconv.FormatInt(used.Swarms, 10), Actors: strconv.FormatInt(used.Actors, 10), CPUMillicores: strconv.FormatInt(used.CPUMillicores, 10), MemoryBytes: strconv.FormatInt(used.MemoryBytes, 10)}
		}
	}
	if err != nil {
		unavailable = "shard resource inventory is unavailable; verify native connectivity and swarm bindings"
	}
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(id))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ID             string       `json:"id"`
		Name           string       `json:"name"`
		Capacity       capacityJSON `json:"capacity"`
		Usage          *usageJSON   `json:"usage"`
		UsageAvailable bool         `json:"usage_available"`
		UsageError     string       `json:"usage_error,omitempty"`
	}{ID: project.ID, Name: project.Name, Capacity: formatCapacity(capacity), Usage: usage, UsageAvailable: usage != nil, UsageError: unavailable})
}
