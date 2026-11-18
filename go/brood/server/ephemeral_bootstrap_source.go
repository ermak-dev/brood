package server

import (
	"context"
	"encoding/hex"
	"strings"

	"brood.dev/go/brood/store"
)

type ephemeralWorkspaceBootstrap struct {
	Commit string `json:"commit"`
}

// Bootstrap читает только immutable pin, не payload. Executor ещё может быть
// provisioning: live authority проверяется у родителя, а не через ready child.
func (s *Server) ephemeralWorkspaceBootstrap(ctx context.Context, g guestRuntime) (*ephemeralWorkspaceBootstrap, error) {
	var result *ephemeralWorkspaceBootstrap
	err := s.store.WithTx(ctx, func(ctx context.Context) error {
		actor, err := s.store.GetManagedActor(ctx, g.uid)
		if err != nil {
			return err
		}
		if actor.Class != store.ActorClassEphemeral || actor.Role != "worker" || actor.Incarnation != g.generation || actor.SwarmID != g.actor.SwarmID || actor.AtespaceUID != g.actor.AtespaceUID || actor.Name != g.actor.Name || actor.Origin != g.actor.Origin || actor.NetworkPolicy != g.actor.NetworkPolicy || actor.Desired != "running" || actor.Operation != "start" || actor.Phase == "deleted" {
			return store.ErrSwarmChildDenied
		}
		intent, err := s.store.GetEphemeralChildIntentForActor(ctx, actor.UID, actor.Incarnation)
		if err != nil {
			return err
		}
		if intent.Parent.Principal.SwarmID != actor.SwarmID || intent.Parent.Principal.AtespaceUID != actor.AtespaceUID || intent.Parent.Principal.ActorUID != actor.ParentUID || intent.PoolUID != actor.PoolUID || intent.Origin != actor.Origin || intent.Template != actor.Template || intent.SourceFingerprint != actor.SourceFingerprint {
			return store.ErrSwarmChildDenied
		}
		swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
		if err != nil {
			return err
		}
		if swarm.ProjectID != g.swarm.ProjectID || swarm.ProjectID != intent.Parent.Principal.ProjectID || swarm.State != "active" || swarm.Source == nil || swarm.Source.Origin != intent.Origin || swarm.Execution != "running" && swarm.Execution != "waiting" {
			return store.ErrSwarmChildDenied
		}
		if err := s.authorizeSwarmOwnerIntent(ctx, swarm.OwnerID, swarm.ID); err != nil {
			return err
		}
		join, err := s.store.GetSwarmChildJoin(ctx, intent.Parent, s.managed.childAuthority)
		if err != nil {
			return err
		}
		if join.State != "open" || join.Interrupted {
			return store.ErrSwarmChildDenied
		}
		pool, err := s.store.GetManagedPool(ctx, actor.PoolUID)
		if err != nil {
			return err
		}
		if pool.State != "enabled" || pool.SourceFingerprint != intent.SourceFingerprint || pool.Template != intent.Template {
			return store.ErrSwarmChildDenied
		}
		first, err := s.store.GetSwarmStep(ctx, swarm.ID, intent.Parent.Pass)
		if err != nil {
			return err
		}
		commit, err := hex.DecodeString(intent.SourceCommit)
		if err != nil || len(commit) != 20 || intent.SourceCommit != strings.ToLower(intent.SourceCommit) || first.SourceCommit != intent.SourceCommit {
			return store.ErrPipelineSourceUnverified
		}
		result = &ephemeralWorkspaceBootstrap{Commit: intent.SourceCommit}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
