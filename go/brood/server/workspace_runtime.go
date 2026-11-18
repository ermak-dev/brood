package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

const workspaceRuntimePrefix = "/api/proxmox/workspace/"

func publicWorkspacePath(path string) bool { return strings.HasPrefix(path, workspaceRuntimePrefix) }

// workspaceOrigin — ответ /api/proxmox/workspace: origin актора и его режим на
// этот запрос, либо оба пустые. GitIdentity (#249) идёт только с origin и
// только при настроенном BROOD_ACTOR_GIT_DOMAIN: без клона ей некуда писаться.
// Models (#263) — модели Claude Code из настроек установки; идут и без origin:
// Claude запускается в каждом акторе, клон ему не нужен.
type workspaceOrigin struct {
	Origin      string                       `json:"origin"`
	Mode        string                       `json:"mode"`
	GitIdentity gitidentity.Identity         `json:"gitIdentity,omitzero"`
	Models      actormodel.Models            `json:"models,omitzero"`
	Ephemeral   *ephemeralWorkspaceBootstrap `json:"ephemeral,omitempty"`
}

// serveGuestWorkspace отдаёт root supervisor гостя источник клона workspace
// (#146). Bootstrap env origin не несёт: прежние guest image отказывают в
// загрузке на незнакомое BROOD_GUEST_* имя. Авторизация — та же, что у
// секретов проекта. Режим пересчитывается на каждом запросе; origin, который
// режим потерял (сменился repository проекта, убран remote), не отдаётся, и
// гость не клонирует.
func (s *Server) serveGuestWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	g, _, ok := s.openGuestRuntime(ctx, w, r, workspaceRuntimePrefix, "workspace origin delivery unavailable", noGuestRuntimeRest)
	if !ok {
		return
	}
	binding, err := s.swarmEvalRuntime(ctx, g.swarm.ID)
	if err != nil {
		writeDetail(w, 503, "eval isolation policy is unavailable")
		return
	}
	if err := s.evalWorkspaceOrigin(ctx, g.swarm.ProjectID, g.actor.Origin, g.actor.NetworkPolicy, binding); err != nil {
		writeDetail(w, 403, "eval workspace requires an explicit network policy and gateway origin with a dedicated candidate project Git credential")
		return
	}
	ephemeral := g.actor.Class == store.ActorClassEphemeral
	var manual *store.ActorOperation
	if ephemeral {
		var err error
		manual, err = s.ephemeralManualBootstrapOrigin(ctx, g)
		if err != nil {
			writeDetail(w, http.StatusForbidden, "ephemeral workspace bootstrap is unavailable")
			return
		}
	}
	automaticEphemeral := ephemeral && manual == nil
	var bootstrap *ephemeralWorkspaceBootstrap
	if automaticEphemeral {
		protocols := r.Header.Values(relay.WorkspaceBootstrapHeader)
		if len(protocols) != 1 || protocols[0] != relay.WorkspaceBootstrapProtocol {
			writeDetail(w, http.StatusForbidden, "ephemeral workspace requires a compatible guest bootstrap protocol")
			return
		}
		var err error
		bootstrap, err = s.ephemeralWorkspaceBootstrap(ctx, g)
		if err != nil {
			writeDetail(w, http.StatusForbidden, "ephemeral workspace bootstrap is unavailable")
			return
		}
	}
	mode, err := s.originMode(ctx, g.swarm.ProjectID, g.actor.Origin, g.actor.NetworkPolicy)
	if err != nil {
		writeDetail(w, 503, "workspace origin delivery unavailable")
		return
	}
	if automaticEphemeral && mode == "" {
		writeDetail(w, http.StatusForbidden, "ephemeral workspace bootstrap is unavailable")
		return
	}
	body := workspaceOrigin{}
	if mode != "" {
		body = workspaceOrigin{Origin: g.actor.Origin, Mode: mode, Ephemeral: bootstrap}
		if s.actorGitDomain != "" {
			// Имя актора проверено при создании; отказ здесь — нарушенный
			// инвариант, и он виден, а не молча оставляет актора без идентичности.
			if body.GitIdentity, err = gitidentity.Actor(g.actor.Name, s.actorGitDomain); err != nil {
				writeDetail(w, 503, "workspace origin delivery unavailable")
				return
			}
		}
	}
	body.Models = s.actorModels
	if binding != nil {
		body.Models = actormodel.Models{Model: binding.Model, Small: binding.Model}
	}
	data, err := json.Marshal(body)
	if err != nil {
		writeDetail(w, 503, "workspace origin delivery unavailable")
		return
	}
	if !s.guestRuntimeCurrent(ctx, g) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	if ephemeral {
		currentManual, err := s.ephemeralManualBootstrapOrigin(ctx, g)
		if err != nil || (currentManual == nil) != (manual == nil) || currentManual != nil && *currentManual != *manual {
			writeDetail(w, http.StatusForbidden, "ephemeral workspace bootstrap is unavailable")
			return
		}
		if automaticEphemeral {
			current, err := s.ephemeralWorkspaceBootstrap(ctx, g)
			if err != nil || current.Commit != bootstrap.Commit {
				writeDetail(w, http.StatusForbidden, "ephemeral workspace bootstrap is unavailable")
				return
			}
		}
	}
	writeGuestRuntime(w, data)
}
