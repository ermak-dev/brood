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

type workspaceOrigin struct {
	Origin        string               `json:"origin"`
	SSHPrivateKey string               `json:"sshPrivateKey"`
	GitIdentity   gitidentity.Identity `json:"gitIdentity,omitzero"`
	Models        actormodel.Models    `json:"models,omitzero"`
}

func (s *Server) serveGuestWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	g, _, ok := s.openGuestRuntime(ctx, w, r, workspaceRuntimePrefix, "workspace origin delivery unavailable", noGuestRuntimeRest)
	if !ok {
		return
	}
	privateKey, err := s.store.GetSwarmSSHPrivateKey(ctx, g.swarm.ID)
	if err != nil {
		writeDetail(w, 503, "swarm SSH identity unavailable")
		return
	}
	body := workspaceOrigin{Origin: g.actor.Origin, SSHPrivateKey: privateKey, Models: s.actorModels}
	if body.Origin != "" {
		if s.actorGitDomain != "" {
			// Имя актора проверено при создании; отказ здесь — нарушенный
			// инвариант, и он виден, а не молча оставляет актора без идентичности.
			if body.GitIdentity, err = gitidentity.Actor(g.actor.Name, s.actorGitDomain); err != nil {
				writeDetail(w, 503, "workspace origin delivery unavailable")
				return
			}
		}
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
