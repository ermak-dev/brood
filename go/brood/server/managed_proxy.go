package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/store"
)

// guestTerminalHost — адрес ttyd внутри агента (proxmox/guest): туда агент
// ведёт поток relay brood-terminal, его же ttyd ждёт в Host и Origin.
const guestTerminalHost = "127.0.0.1:8080"

// resolveTerminal выбирает агента роя для прокси терминала: UID и
// поколение берутся из базы, запрос на них не влияет.
func (m *managedProvider) resolveTerminal(swarmID, name string) (proxy.Target, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := m.actorByName(ctx, swarmID, name)
	if errors.Is(err, store.ErrNotFound) {
		return proxy.Target{}, proxy.ErrUnknownAgent
	}
	if err != nil {
		return proxy.Target{}, err
	}
	target := proxy.Target{UID: a.UID, Generation: a.Incarnation}
	if !m.validTerminal(target) {
		return proxy.Target{}, &app.APIError{Status: http.StatusServiceUnavailable, Detail: "the agent terminal is not ready; start the agent if it is stopped"}
	}
	return target, nil
}

// terminalAgent — агент цели, если это поколение ещё работает и на связи.
func (m *managedProvider) terminalAgent(ctx context.Context, target proxy.Target) (store.ManagedActor, error) {
	a, err := m.server.store.GetManagedActor(ctx, target.UID)
	if err != nil {
		return a, err
	}
	if sw, err := m.server.store.GetSwarm(ctx, a.SwarmID); err != nil || sw.State != "active" {
		return a, proxy.ErrUnknownAgent
	}
	verified, ok := m.verified.Load(target.UID)
	if !ok || verified != target.Generation || a.Phase != "running" || a.Desired != "running" || a.Incarnation != target.Generation || m.hub == nil || !m.hub.Connected(target.UID, target.Generation) {
		return a, proxy.ErrUnknownAgent
	}
	p, err := m.server.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		return a, err
	}
	if !m.poolAvailable(p) {
		return a, proxy.ErrUnknownAgent
	}
	return a, nil
}

func (m *managedProvider) validTerminal(target proxy.Target) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := m.terminalAgent(ctx, target)
	return err == nil
}

func (m *managedProvider) dialTerminal(ctx context.Context, target proxy.Target) (net.Conn, error) {
	a, err := m.terminalAgent(ctx, target)
	if err != nil {
		return nil, err
	}
	return m.hub.Dial(ctx, a.UID, a.Incarnation)
}

func (s *Server) guestRoute(w http.ResponseWriter, r *http.Request) {
	if s.managed.hub == nil {
		writeDetail(w, 503, "Proxmox guest relay is not configured")
		return
	}
	s.managed.hub.Handler().ServeHTTP(w, r)
}
func publicGuestPath(path string) bool { return strings.HasPrefix(path, "/api/proxmox/guest/") }

// agentViewBinding — UID и поколение работающего агента, к которым
// привязан пропуск терминала.
func (s *Server) agentViewBinding(ctx context.Context, swarm, name string) (string, int64, error) {
	a, err := s.managed.actorByName(ctx, swarm, name)
	if err != nil {
		return "", 0, err
	}
	if a.Phase != "running" || a.Desired != "running" {
		return "", 0, proxy.ErrUnknownAgent
	}
	if sw, err := s.store.GetSwarm(ctx, swarm); err != nil || sw.State != "active" {
		return "", 0, proxy.ErrUnknownAgent
	}
	return a.UID, a.Incarnation, nil
}

// validAgentViewBinding — пропуск ведёт к тому же поколению агента, а
// пропуск с вводом — ещё и к рою, которым пользователь управляет.
func (s *Server) validAgentViewBinding(ctx context.Context, v agentView) bool {
	uid, generation, err := s.agentViewBinding(ctx, v.swarm, v.agent)
	if err != nil || uid != v.uid || generation != v.generation {
		return false
	}
	return v.readOnly || s.authorizeSwarmOwnerIntent(ctx, v.userID, v.swarm) == nil
}
