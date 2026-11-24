package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// Лимиты проекта в API: десятичные строки, null — без предела.

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

func formatCapacity(limits store.ProjectCapacity) capacityJSON {
	return capacityJSON{MaxSwarms: limitString(limits.MaxSwarms), MaxActors: limitString(limits.MaxActors), MaxCPUMillicores: limitString(limits.MaxCPUMillicores), MaxMemoryBytes: limitString(limits.MaxMemoryBytes)}
}

func parseCapacity(body capacityJSON) (store.ProjectCapacity, error) {
	result := store.ProjectCapacity{}
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

// handleProjectLimits — PUT /api/projects/{id}/capacity: лимиты проекта,
// только админ.
func (s *Server) handleProjectLimits(w http.ResponseWriter, r *http.Request) {
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
	limits, err := parseCapacity(body)
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.app.SetProjectLimits(r.Context(), user.ID, r.PathValue("id"), limits); err != nil {
		apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProjectDetail — GET /api/projects/{id}: проект, его лимиты и
// занятое. Занятое считается по агентам роёв; сбой подсчёта не скрывает
// проект, а отдаётся причиной в usage_error.
func (s *Server) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	limits, err := s.app.ProjectLimits(ctx, user.ID, id)
	if errors.Is(err, app.ErrNotAllowed) {
		err = app.ErrNotFound
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		apiError(w, r, err)
		return
	}
	rows, err := s.store.ListSwarmsByProject(ctx, id)
	if err != nil {
		apiError(w, r, err)
		return
	}
	swarms := app.SwarmsOf(rows)
	var usage *usageJSON
	unavailable := ""
	agents, err := s.agentReservations(ctx, swarms)
	if err == nil {
		var used app.ProjectUsage
		used, err = app.ProjectUsageOf(id, swarms, agents)
		if err == nil {
			usage = &usageJSON{Swarms: strconv.FormatInt(used.Swarms, 10), Actors: strconv.FormatInt(used.Actors, 10), CPUMillicores: strconv.FormatInt(used.CPUMillicores, 10), MemoryBytes: strconv.FormatInt(used.MemoryBytes, 10)}
		}
	}
	if err != nil {
		unavailable = "project usage is unavailable: the agents of its swarms could not be counted; reload the page"
	}
	allowed, err := s.app.Can(ctx, user.ID, app.ActionViewProject, app.ProjectRes(id))
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
	}{ID: project.ID, Name: project.Name, Capacity: formatCapacity(limits), Usage: usage, UsageAvailable: usage != nil, UsageError: unavailable})
}
