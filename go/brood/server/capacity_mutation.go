package server

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

type nativeWorkloadOperation struct {
	server        *Server
	context       context.Context
	swarm         app.Swarm
	action        string
	actor         *ateapi.Actor
	template      *ateapi.ActorTemplate
	name          string
	currentActor  *ateapi.Actor
	savedActor    *ateapi.Actor
	savedTemplate *ateapi.ActorTemplate
}

func (o *nativeWorkloadOperation) boundNamespace() error {
	space, err := o.server.ateapi.GetAtespace(o.context, o.swarm.AtespaceName)
	if err != nil {
		return err
	}
	if o.swarm.AtespaceUID == "" || space.GetMetadata().GetUid() != o.swarm.AtespaceUID || space.GetMetadata().GetName() != o.swarm.AtespaceName {
		return fmt.Errorf("%w: swarm namespace identity changed", app.ErrConflict)
	}
	if space.GetMetadata().GetDeleteTime() != nil {
		return fmt.Errorf("%w: swarm namespace is terminating", app.ErrConflict)
	}
	return nil
}

func (o *nativeWorkloadOperation) Prepare(swarms []app.Swarm) (app.WorkloadAdmission, error) {
	var plan app.WorkloadAdmission
	if err := o.authorizeWrite(); err != nil {
		return plan, err
	}
	if err := o.boundNamespace(); err != nil {
		return plan, err
	}
	capacity, err := o.server.gateway.ProjectCapacity(o.swarm.ProjectID)
	if err != nil {
		return plan, err
	}
	var template *ateapi.ActorTemplate
	if o.action == "delete-template" {
		current, err := o.server.ateapi.GetActorTemplate(o.context, o.swarm.AtespaceName, o.name)
		if err != nil {
			return plan, err
		}
		actors, err := o.server.ateapi.ListActors(o.context, o.swarm.AtespaceName)
		if err != nil {
			return plan, err
		}
		golden := current.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED
		for _, actor := range actors {
			if actor.GetActorTemplate().GetAtespace() != o.swarm.AtespaceName || actor.GetActorTemplate().GetName() != o.name {
				continue
			}
			if golden && current.GetMetadata().GetUid() != "" && actor.GetMetadata().GetName() == current.GetMetadata().GetUid() {
				continue
			}
			return plan, fmt.Errorf("%w: template is still referenced by an actor; update or delete the actor first", app.ErrConflict)
		}
		plan.Name, plan.TemplateOnly = o.name, true
		return plan, nil
	}
	if o.action == "create-template" {
		template = o.template
		plan.Name = template.GetMetadata().GetName()
		plan.WorkerSelector = maps.Clone(template.GetWorkerSelector().GetMatchLabels())
		plan.TemplateOnly = !goldenPending(template)
		if !plan.TemplateOnly {
			plan.Name = pendingGoldenPrefix + plan.Name
		}
	} else {
		plan.Name = o.name
		plan.Replace = o.action != "create-actor"
		actor := o.actor
		if plan.Replace {
			o.currentActor, err = o.server.ateapi.GetActor(o.context, o.swarm.AtespaceName, o.name)
			if err != nil {
				return plan, err
			}
			if o.action == "resume-actor" {
				actor = proto.CloneOf(o.currentActor)
			}
		}
		template, err = o.server.ateapi.GetActorTemplate(o.context, o.swarm.AtespaceName, actor.GetActorTemplate().GetName())
		if err != nil {
			return plan, err
		}
		plan.WorkerSelector = maps.Clone(actor.GetWorkerSelector().GetMatchLabels())
		for key, value := range capacity.WorkerSelector {
			if selected, ok := template.GetWorkerSelector().GetMatchLabels()[key]; ok && selected != value {
				return plan, fmt.Errorf("%w: actor template selects workers outside the shard", app.ErrInvalidWorkload)
			}
		}
	}
	if !plan.TemplateOnly {
		limits := template.GetResources().GetLimits()
		if limits.GetCpuMillicores() <= 0 || limits.GetMemoryBytes() <= 0 {
			return plan, fmt.Errorf("%w: template must have positive resolved CPU and memory limits", app.ErrInvalidWorkload)
		}
		plan.CPUMillicores, plan.MemoryBytes = limits.GetCpuMillicores(), limits.GetMemoryBytes()
	}
	constrained := capacity.MaxActors != nil || capacity.MaxCPUMillicores != nil || capacity.MaxMemoryBytes != nil
	if constrained && !plan.TemplateOnly {
		plan.Reservations, err = nativeShardReservations(o.context, o.server.ateapi, swarms)
		if err != nil {
			return plan, err
		}
	} else if plan.Replace {
		previous := app.ActorReservation{Name: o.name, SwarmID: o.swarm.ID}
		old, readErr := o.server.ateapi.GetActorTemplate(o.context, o.swarm.AtespaceName, o.currentActor.GetActorTemplate().GetName())
		if readErr == nil {
			previous.CPUMillicores = old.GetResources().GetLimits().GetCpuMillicores()
			previous.MemoryBytes = old.GetResources().GetLimits().GetMemoryBytes()
		} else {
			var apiErr *substrate.APIError
			if !errors.As(readErr, &apiErr) || apiErr.Status != 404 {
				return plan, readErr
			}
		}
		plan.Reservations = []app.ActorReservation{previous}
	}
	return plan, nil
}

func workerSelector(labels map[string]string) *ateapi.Selector {
	if len(labels) == 0 {
		return nil
	}
	return &ateapi.Selector{MatchLabels: maps.Clone(labels)}
}

func (o *nativeWorkloadOperation) authorizeWrite() error {
	user, ok := auth.UserFromContext(o.context)
	if !ok {
		return app.ErrNotAllowed
	}
	allowed, err := o.server.app.Can(user.ID, app.ActionManageSwarm, app.SwarmRes(o.swarm.ID))
	if err != nil {
		return err
	}
	if !allowed {
		return app.ErrNotAllowed
	}
	if o.template != nil && o.template.GoldenEgressPolicy != nil {
		current, err := o.server.store.GetUser(o.context, user.ID)
		if err != nil {
			return err
		}
		if current.Disabled || !current.IsAdmin {
			return app.ErrNotAllowed
		}
	}
	return nil
}

func (o *nativeWorkloadOperation) Apply(labels map[string]string) error {
	if err := o.boundNamespace(); err != nil {
		return err
	}
	if err := o.authorizeWrite(); err != nil {
		return err
	}
	var err error
	switch o.action {
	case "create-actor":
		o.actor.WorkerSelector = workerSelector(labels)
		o.savedActor, err = o.server.ateapi.CreateActor(o.context, o.actor)
	case "update-actor":
		o.actor.WorkerSelector = workerSelector(labels)
		o.savedActor, err = o.server.ateapi.UpdateActor(o.context, o.actor)
	case "resume-actor":
		selector := workerSelector(labels)
		if !proto.Equal(o.currentActor.GetWorkerSelector(), selector) {
			updated := proto.CloneOf(o.currentActor)
			updated.WorkerSelector = selector
			if _, err = o.server.ateapi.UpdateActor(o.context, updated); err != nil {
				return err
			}
		}
		if err := o.authorizeWrite(); err != nil {
			return err
		}
		o.savedActor, err = o.server.ateapi.ResumeActor(o.context, o.swarm.AtespaceName, o.name)
	case "delete-template":
		err = o.server.ateapi.DeleteActorTemplate(o.context, o.swarm.AtespaceName, o.name)
	case "create-template":
		o.template.WorkerSelector = workerSelector(labels)
		o.savedTemplate, err = o.server.ateapi.CreateActorTemplate(o.context, o.template)
	default:
		return fmt.Errorf("%w: unsupported native workload operation", app.ErrInvalidWorkload)
	}
	return err
}
