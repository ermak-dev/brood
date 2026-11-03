package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const workspaceRuntimePrefix = "/api/proxmox/workspace/"

func publicWorkspacePath(path string) bool { return strings.HasPrefix(path, workspaceRuntimePrefix) }

// workspaceOrigin — ответ /api/proxmox/workspace: origin актора и его режим на
// этот запрос, либо оба пустые.
type workspaceOrigin struct {
	Origin string `json:"origin"`
	Mode   string `json:"mode"`
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
	writeGuestRuntime(w, data)
}
