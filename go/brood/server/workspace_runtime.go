package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/gitidentity"
)

const workspaceRuntimePrefix = "/api/proxmox/workspace/"

func publicWorkspacePath(path string) bool { return strings.HasPrefix(path, workspaceRuntimePrefix) }

// workspaceOrigin — ответ /api/proxmox/workspace: origin актора и его режим на
// этот запрос, либо оба пустые. GitIdentity (#249) идёт только с origin и
// только при настроенном BROOD_ACTOR_GIT_DOMAIN: без клона ей некуда писаться.
// Models (#263) — модели Claude Code из настроек установки; идут и без origin:
// Claude запускается в каждом акторе, клон ему не нужен.
type workspaceOrigin struct {
	Origin      string               `json:"origin"`
	Mode        string               `json:"mode"`
	GitIdentity gitidentity.Identity `json:"gitIdentity,omitzero"`
	Models      actormodel.Models    `json:"models,omitzero"`
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
	mode, err := s.originMode(ctx, g.swarm.ProjectID, g.actor.Origin)
	if err != nil {
		writeDetail(w, 503, "workspace origin delivery unavailable")
		return
	}
	body := workspaceOrigin{}
	if mode != "" {
		body = workspaceOrigin{Origin: g.actor.Origin, Mode: mode}
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
	data, err := json.Marshal(body)
	if err != nil {
		writeDetail(w, 503, "workspace origin delivery unavailable")
		return
	}
	if !s.guestRuntimeCurrent(ctx, g) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	writeGuestRuntime(w, data)
}
