package server

import (
	"errors"
	"net/http"
	"strconv"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/substrate"
)

func (s *Server) writePlacementFailure(w http.ResponseWriter, r *http.Request, user store.User, sw app.Swarm, name string, failure error) bool {
	var native *substrate.APIError
	if !errors.As(failure, &native) || native.Detail != "no worker has room for the actor" {
		return false
	}
	response := map[string]any{
		"code":   "actor_placement_unavailable",
		"detail": "No worker can currently host this actor. Ask an administrator to check worker availability, placement labels, and free resources.",
	}
	current, err := s.store.GetUser(r.Context(), user.ID)
	if err == nil && current.IsAdmin && !current.Disabled {
		report, readErr := s.placementReport(r, sw, name)
		if readErr == nil {
			current, err = s.store.GetUser(r.Context(), user.ID)
			if err == nil && current.IsAdmin && !current.Disabled {
				response["detail"] = "No worker can currently host this actor. " + report.diagnostic.String()
				response["placement"] = report.public()
			}
		} else {
			response["detail"] = "No worker can currently host this actor. Worker diagnostics are unavailable; refresh the worker list to investigate."
		}
	}
	writeJSON(w, native.Status, response)
	return true
}

type placementReport struct {
	diagnostic  substrate.PlacementDiagnostic
	constraints substrate.PlacementConstraints
}

func (s *Server) placementReport(r *http.Request, sw app.Swarm, name string) (placementReport, error) {
	var report placementReport
	ctx := r.Context()
	if err := s.checkSwarmNamespace(ctx, sw); err != nil {
		return report, err
	}
	actor, err := s.ateapi.GetActor(ctx, sw.AtespaceName, name)
	if err != nil {
		return report, err
	}
	template, err := s.ateapi.GetActorTemplate(ctx, sw.AtespaceName, actor.GetActorTemplate().GetName())
	if err != nil {
		return report, err
	}
	workers, err := s.ateapi.ListWorkers(ctx)
	if err != nil {
		return report, err
	}
	if err := s.checkSwarmNamespace(ctx, sw); err != nil {
		return report, err
	}
	report.constraints = substrate.PlacementConstraints{Limits: template.GetResources().GetLimits(), TemplateSelector: template.GetWorkerSelector(), ActorSelector: actor.GetWorkerSelector()}
	report.diagnostic = substrate.DiagnosePlacement(workers, report.constraints)
	return report, nil
}

func (p placementReport) public() map[string]any {
	workers := make([]map[string]any, 0, len(p.diagnostic.Workers))
	for _, worker := range p.diagnostic.Workers {
		reasons := make([]map[string]any, 0, len(worker.Reasons))
		for _, reason := range worker.Reasons {
			value := map[string]any{"code": reason.Code}
			switch reason.Code {
			case substrate.PlacementInactive:
				value["state"] = reason.State.String()
			case substrate.PlacementCPU, substrate.PlacementMemory, substrate.PlacementActorSlots:
				value["required"] = strconv.FormatInt(reason.Required, 10)
				value["available"] = strconv.FormatInt(reason.Available, 10)
			}
			reasons = append(reasons, value)
		}
		workers = append(workers, map[string]any{"name": worker.Name, "reasons": reasons})
	}
	return map[string]any{
		"workers": workers, "noWorkers": p.diagnostic.NoWorkers,
		"templateSelector": p.constraints.TemplateSelector.GetMatchLabels(),
		"actorSelector":    p.constraints.ActorSelector.GetMatchLabels(),
		"unchecked":        p.diagnostic.Unchecked,
	}
}
