package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

func (s *Server) mountProxmoxConnections() {
	s.mux.HandleFunc("GET /api/proxmox-connections", s.adminProxmoxConnections(func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.store.ListConnections(r.Context())
		if err != nil {
			writeDetail(w, 500, "Proxmox connections unavailable")
			return
		}
		items := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			items = append(items, connectionDTO(c))
		}
		writeJSON(w, 200, map[string]any{"items": items})
	}))
	s.mux.HandleFunc("POST /api/proxmox-connections", s.adminProxmoxConnections(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeDetail(w, 400, "invalid connection payload")
			return
		}
		fields, err := proxmoxObject(string(raw))
		if err != nil {
			writeDetail(w, 400, "invalid connection payload")
			return
		}
		for key := range fields {
			switch key {
			case "name", "endpoint", "tokenID", "tokenSecret", "caPEM", "node", "resourcePool", "storage", "network", "template", "templateDigest", "capacity":
			default:
				writeDetail(w, 400, "invalid connection payload")
				return
			}
		}
		c, err := parseProxmoxConnection(fields)
		if err != nil {
			writeDetail(w, 422, err.Error())
			return
		}
		// Общий admission lock сериализует активацию и удаление с созданием.
		s.provider.admission.Lock()
		defer s.provider.admission.Unlock()
		if s.provider.registry != nil {
			if _, exists := s.provider.registry.pools[c.Name]; exists {
				writeDetail(w, 409, "connection name already exists in environment configuration")
				return
			}
		}
		c, err = s.store.CreateConnection(r.Context(), c)
		if err != nil {
			writeDetail(w, 409, "connection name already exists or is invalid")
			return
		}
		writeJSON(w, 201, connectionDTO(c))
	}))
	s.mux.HandleFunc("POST /api/proxmox-connections/{uid}/clone", s.adminProxmoxConnections(s.cloneProxmoxConnection))
	s.mux.HandleFunc("DELETE /api/proxmox-connections/{uid}", s.adminProxmoxConnections(func(w http.ResponseWriter, r *http.Request) {
		s.provider.admission.Lock()
		defer s.provider.admission.Unlock()
		if err := s.store.DeleteConnection(r.Context(), r.PathValue("uid")); err != nil {
			switch {
			case errors.Is(err, store.ErrInUse):
				writeDetail(w, 409, "connection still has a pool or agents")
			case errors.Is(err, store.ErrNotFound):
				writeDetail(w, 404, "connection not found")
			default:
				writeDetail(w, 500, "Proxmox connection deletion failed")
			}
			return
		}
		w.WriteHeader(204)
	}))
}

func (s *Server) cloneProxmoxConnection(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeDetail(w, 400, "invalid clone payload")
		return
	}
	fields, err := proxmoxObject(string(raw))
	if err != nil {
		writeDetail(w, 400, "invalid clone payload")
		return
	}
	for key := range fields {
		switch key {
		case "name", "template", "templateDigest", "capacity":
		default:
			writeDetail(w, 400, "invalid clone payload")
			return
		}
	}
	for _, key := range []string{"name", "template", "templateDigest", "capacity"} {
		if _, ok := fields[key]; !ok {
			writeDetail(w, 422, "clone requires name, template, templateDigest and capacity")
			return
		}
	}
	// Тот же lock защищает UID источника от удаления и сериализует admission.
	s.provider.admission.Lock()
	defer s.provider.admission.Unlock()
	rows, err := s.store.ListConnections(r.Context())
	if err != nil {
		writeDetail(w, 500, "Proxmox connections unavailable")
		return
	}
	var source *store.Connection
	for i := range rows {
		if rows[i].UID == r.PathValue("uid") {
			source = &rows[i]
			break
		}
	}
	if source == nil {
		writeDetail(w, 404, "connection not found")
		return
	}
	secret, err := s.store.ConnectionSecret(r.Context(), source.Name)
	if err != nil {
		writeDetail(w, 500, "Proxmox connection unavailable")
		return
	}
	// Только сервер дополняет разрешённый ввод credential и областью источника.
	for key, value := range map[string]string{
		"endpoint": source.Endpoint, "tokenID": source.TokenID, "tokenSecret": secret,
		"caPEM": source.CAPEM, "node": source.Node, "resourcePool": source.ResourcePool,
		"storage": source.Storage, "network": source.Network,
	} {
		fields[key], _ = json.Marshal(value)
	}
	c, err := parseProxmoxConnection(fields)
	if err != nil {
		writeDetail(w, 422, err.Error())
		return
	}
	if s.provider.registry != nil {
		if _, exists := s.provider.registry.pools[c.Name]; exists {
			writeDetail(w, 409, "connection name already exists in environment configuration")
			return
		}
	}
	// Middleware мог проверить сессию до ожидания тела или admission lock.
	// Перед записью нужны действующая сессия и текущая роль из БД, не snapshot.
	if !s.validProxySession(r) {
		writeDetail(w, 401, "authentication required")
		return
	}
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetUser(r.Context(), user.ID)
	if err != nil || current.Disabled {
		writeDetail(w, 401, "authentication required")
		return
	}
	if !current.IsAdmin {
		writeDetail(w, 403, "administrator access required")
		return
	}
	c, err = s.store.CreateConnection(r.Context(), c)
	if err != nil {
		writeDetail(w, 409, "connection name already exists or is invalid")
		return
	}
	writeJSON(w, 201, connectionDTO(c))
}

func (s *Server) adminProxmoxConnections(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := currentUser(w, r)
		if !ok {
			return
		}
		if !user.IsAdmin {
			writeDetail(w, 403, "administrator access required")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") == "" {
			writeDetail(w, 403, "same-origin request required")
			return
		}
		next(w, r)
	}
}

func parseProxmoxConnection(fields map[string]json.RawMessage) (store.Connection, error) {
	var c store.Connection
	if json.Unmarshal(fields["name"], &c.Name) != nil || len(c.Name) > 63 || !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`).MatchString(c.Name) {
		return c, errors.New("name must be a lowercase DNS label of at most 63 characters")
	}
	if json.Unmarshal(fields["tokenSecret"], &c.TokenSecret) != nil || !proxmoxHeaderValue(c.TokenSecret) {
		return c, errors.New("tokenSecret must be a header-safe secret")
	}
	delete(fields, "name")
	delete(fields, "tokenSecret")
	fields["credentialRef"] = json.RawMessage(`"form"`)
	raw, _ := json.Marshal(fields)
	cfg, err := parseProxmoxPool(raw)
	if err != nil {
		return c, err
	}
	if cfg.runtime != nil && len(c.Name) > 48 {
		return c, errors.New("runtime pool names must be lowercase DNS labels of at most 48 characters")
	}
	runtime := make(map[string]json.RawMessage)
	for _, key := range []string{"template", "templateDigest", "capacity"} {
		if value, ok := fields[key]; ok {
			runtime[key] = value
		}
	}
	if len(runtime) != 0 {
		data, _ := json.Marshal(runtime)
		c.RuntimeJSON = string(data)
	}
	c.Endpoint, c.TokenID, c.CAPEM = cfg.endpoint, cfg.tokenID, cfg.caPEM
	c.Node, c.ResourcePool, c.Storage, c.Network = cfg.node, cfg.resourcePool, cfg.storage, cfg.network
	return c, nil
}

func connectionDTO(c store.Connection) map[string]any {
	return map[string]any{"uid": c.UID, "name": c.Name, "endpoint": c.Endpoint, "tokenID": c.TokenID, "node": c.Node, "resourcePool": c.ResourcePool, "storage": c.Storage, "network": c.Network, "credentialConfigured": true}
}

func connectionConfig(c store.Connection) (proxmoxPool, error) {
	cfg := proxmoxPool{endpoint: c.Endpoint, tokenID: c.TokenID, caPEM: c.CAPEM, node: c.Node, resourcePool: c.ResourcePool, storage: c.Storage, network: c.Network}
	if c.RuntimeJSON != "" {
		fields, err := proxmoxObject(c.RuntimeJSON)
		if err != nil {
			return cfg, errors.New("stored runtime configuration is invalid")
		}
		cfg.runtime, err = parseProxmoxRuntime(fields)
		if err != nil {
			return cfg, errors.New("stored runtime configuration is invalid")
		}
	}
	return cfg, nil
}

// configuredConnection читает БД на запрос, не изменяя immutable env registry.
func (m *provider) configuredConnection(ctx context.Context, name string) (proxmoxPool, error) {
	var env proxmoxPool
	var exists bool
	if m.registry != nil {
		env, exists = m.registry.pools[name]
	}
	rows, err := m.server.store.ListConnections(ctx)
	if err != nil {
		return proxmoxPool{}, errors.New("Proxmox connections unavailable")
	}
	for _, c := range rows {
		if c.Name != name {
			continue
		}
		if exists {
			return proxmoxPool{}, fmt.Errorf("%w: connection name conflicts with environment configuration", app.ErrConflict)
		}
		return connectionConfig(c)
	}
	if exists {
		return env, nil
	}
	return proxmoxPool{}, app.ErrNotFound
}

func (m *provider) connectionItems(ctx context.Context) ([]proxmoxPoolDTO, error) {
	rows, err := m.server.store.ListConnections(ctx)
	if err != nil {
		return nil, errors.New("Proxmox connections unavailable")
	}
	items := make([]proxmoxPoolDTO, 0, len(rows))
	for _, c := range rows {
		cfg, err := m.configuredConnection(ctx, c.Name)
		if err != nil {
			return nil, err
		}
		available := cfg.runtime != nil && m.registry != nil && m.registry.publicOrigin != "" && len(m.registry.relaySecret) >= 32
		items = append(items, proxmoxPoolDTO{Name: c.Name, Endpoint: c.Endpoint, TokenID: c.TokenID, Node: c.Node, ResourcePool: c.ResourcePool, Storage: c.Storage, Network: c.Network, CredentialConfigured: true, RuntimeAvailable: available})
	}
	return items, nil
}
