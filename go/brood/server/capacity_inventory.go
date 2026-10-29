package server

import (
	"context"
	"errors"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

const pendingGoldenPrefix = "@golden:"

type shardInventoryError struct{ cause error }

func (e *shardInventoryError) Error() string {
	return "shard resource inventory is unavailable; verify native connectivity and swarm bindings"
}

func templateReservation(swarmID, name string, template *ateapi.ActorTemplate) (app.ActorReservation, error) {
	limits := template.GetResources().GetLimits()
	if limits.GetCpuMillicores() <= 0 || limits.GetMemoryBytes() <= 0 {
		return app.ActorReservation{}, &shardInventoryError{cause: errors.New("template resource limits are unresolved")}
	}
	return app.ActorReservation{Name: name, SwarmID: swarmID, CPUMillicores: limits.GetCpuMillicores(), MemoryBytes: limits.GetMemoryBytes()}, nil
}

func goldenPending(template *ateapi.ActorTemplate) bool {
	if template.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() == ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED {
		return false
	}
	status := template.GetStatus().GetGoldenSnapshotStatus()
	return status.GetGoldenTag() == nil && status.GetErrorMessage() == ""
}

func nativeShardReservations(ctx context.Context, client *substrate.Client, swarms []app.Swarm) ([]app.ActorReservation, error) {
	reservations := []app.ActorReservation{}
	if client == nil {
		return nil, &shardInventoryError{cause: errors.New("substrate is not configured")}
	}
	for _, swarm := range swarms {
		if swarm.State == app.StateDeleted {
			continue
		}
		if err := checkNativeNamespace(ctx, client, swarm); err != nil {
			return nil, &shardInventoryError{cause: err}
		}
		templates, err := client.ListActorTemplates(ctx, swarm.AtespaceName)
		if err != nil {
			return nil, &shardInventoryError{cause: err}
		}
		byName := make(map[string]*ateapi.ActorTemplate, len(templates))
		pendingUIDs := make(map[string]bool)
		for _, template := range templates {
			name := template.GetMetadata().GetName()
			if name == "" || template.GetMetadata().GetAtespace() != swarm.AtespaceName {
				return nil, &shardInventoryError{cause: errors.New("invalid template identity in native inventory")}
			}
			byName[name] = template
			if goldenPending(template) {
				uid := template.GetMetadata().GetUid()
				if uid == "" {
					return nil, &shardInventoryError{cause: errors.New("pending golden template has no UID")}
				}
				reservation, err := templateReservation(swarm.ID, pendingGoldenPrefix+name, template)
				if err != nil {
					return nil, err
				}
				reservations = append(reservations, reservation)
				pendingUIDs[uid] = true
			}
		}
		actors, err := client.ListActors(ctx, swarm.AtespaceName)
		if err != nil {
			return nil, &shardInventoryError{cause: err}
		}
		if err := checkNativeNamespace(ctx, client, swarm); err != nil {
			return nil, &shardInventoryError{cause: err}
		}
		for _, actor := range actors {
			name := actor.GetMetadata().GetName()
			if name == "" || actor.GetMetadata().GetAtespace() != swarm.AtespaceName {
				return nil, &shardInventoryError{cause: errors.New("invalid actor identity in native inventory")}
			}
			template := byName[actor.GetActorTemplate().GetName()]
			if template == nil || actor.GetActorTemplate().GetAtespace() != swarm.AtespaceName {
				return nil, &shardInventoryError{cause: errors.New("actor references an unknown template")}
			}
			if pendingUIDs[name] && template.GetMetadata().GetUid() == name {
				continue
			}
			reservation, err := templateReservation(swarm.ID, name, template)
			if err != nil {
				return nil, err
			}
			reservations = append(reservations, reservation)
		}
	}
	return reservations, nil
}
