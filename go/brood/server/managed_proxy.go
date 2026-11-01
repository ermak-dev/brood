package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/substrate"
)

type managedResolver struct {
	server *Server
	native proxy.UpstreamResolver
}

func (r *managedResolver) ActorUpstream(atespace, name string) (*url.URL, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sw, err := r.server.store.GetSwarmByAtespace(ctx, atespace)
	if err != nil {
		return nil, err
	}
	a, err := r.server.managed.actorByName(ctx, sw.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return r.native.ActorUpstream(atespace, name)
	}
	if err != nil {
		return nil, err
	}
	if sw.AtespaceUID == nil || a.AtespaceUID != *sw.AtespaceUID {
		return nil, proxy.ErrUnknownActor
	}
	if err := r.server.managedNameAvailable(ctx, atespace, name); err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "http", Host: a.UID + "." + managedDecimal(a.Incarnation) + ".proxmox.invalid:8080"}
	if !r.server.managed.validTarget(target) {
		return nil, &substrate.APIError{Status: 503, Detail: "managed terminal is not ready; explicitly resume the actor if stopped"}
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
	sw, err := m.server.store.GetSwarm(ctx, a.SwarmID)
	if err != nil || sw.AtespaceUID == nil || a.AtespaceUID != *sw.AtespaceUID {
		return a, proxy.ErrUnknownActor
	}
	if err := m.server.managedNameAvailable(ctx, sw.AtespaceName, a.Name); err != nil {
		return a, err
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
	sw, err := s.store.GetSwarm(ctx, swarm)
	if err != nil || sw.AtespaceUID == nil || a.AtespaceUID != *sw.AtespaceUID {
		return "", 0, proxy.ErrUnknownActor
	}
	if err := s.managedNameAvailable(ctx, sw.AtespaceName, name); err != nil {
		return "", 0, err
	}
	return a.UID, a.Incarnation, nil
}
func (s *Server) managedNameAvailable(ctx context.Context, atespace, name string) error {
	if s.ateapi == nil {
		return proxy.ErrUnknownActor
	}
	_, err := s.ateapi.GetActor(ctx, atespace, name)
	if err == nil {
		return &substrate.APIError{Status: 409, Detail: "native actor conflicts with managed allocation"}
	}
	var api *substrate.APIError
	if errors.As(err, &api) && api.Status == 404 {
		return nil
	}
	return err
}

func (s *Server) validManagedView(ctx context.Context, v actorView) bool {
	uid, generation, err := s.managedViewBinding(ctx, v.swarm, v.actor)
	return err == nil && uid == v.managedUID && generation == v.generation
}
