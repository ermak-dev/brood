package server

import (
	"brood.dev/go/brood/app"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/store"
)

type managedResolver struct {
	server *Server
}

func (r *managedResolver) ActorUpstream(swarmID, name string) (*url.URL, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := r.server.managed.actorByName(ctx, swarmID, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, proxy.ErrUnknownActor
	}
	if err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "http", Host: a.UID + "." + managedDecimal(a.Incarnation) + ".proxmox.invalid:8080"}
	if !r.server.managed.validTarget(target) {
		return nil, &app.APIError{Status: 503, Detail: "managed terminal is not ready; explicitly resume the actor if stopped"}
	}
	return target, nil
}

func (m *managedProvider) targetActor(ctx context.Context, host string) (store.ManagedActor, error) {
	uid, generation, ok := managedGeneration(host)
	if !ok {
		return store.ManagedActor{}, proxy.ErrUnknownActor
	}
	a, err := m.server.store.GetManagedActor(ctx, uid)
	if err != nil {
		return a, err
	}
	if sw, err := m.server.store.GetSwarm(ctx, a.SwarmID); err != nil || sw.State != "active" {
		return a, proxy.ErrUnknownActor
	}
	verified, ok := m.verified.Load(uid)
	if !ok || verified != generation || a.Phase != "running" || a.Desired != "running" || a.Incarnation != generation || m.hub == nil || !m.hub.Connected(uid, generation) {
		return a, proxy.ErrUnknownActor
	}
	p, err := m.server.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		return a, err
	}
	if p.SourceFingerprint != a.SourceFingerprint || !m.poolAvailable(p) {
		return a, proxy.ErrUnknownActor
	}
	return a, nil
}
func (m *managedProvider) validTarget(target *url.URL) bool {
	if target.Scheme == "https" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := m.targetActor(ctx, target.Host)
	return err == nil
}
func (m *managedProvider) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" {
		return nil, proxy.ErrUnknownActor
	}
	a, err := m.targetActor(ctx, address)
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

func (s *Server) managedViewBinding(ctx context.Context, swarm, name string) (string, int64, error) {
	a, err := s.managed.actorByName(ctx, swarm, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if a.Phase != "running" || a.Desired != "running" {
		return "", 0, proxy.ErrUnknownActor
	}
	if sw, err := s.store.GetSwarm(ctx, swarm); err != nil || sw.State != "active" {
		return "", 0, proxy.ErrUnknownActor
	}
	return a.UID, a.Incarnation, nil
}
func (s *Server) validManagedView(ctx context.Context, v actorView) bool {
	uid, generation, err := s.managedViewBinding(ctx, v.swarm, v.actor)
	if err != nil || uid != v.managedUID || generation != v.generation {
		return false
	}
	return v.readOnly || uid == "" || s.authorizeSwarmOwnerIntent(ctx, v.userID, v.swarm) == nil
}
