package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"brood.dev/go/brood/store"
)

// Админский API подключений Proxmox (#118): форма настройки пула пишет сюда
// endpoint, tokenID, секрет, CA и координаты PVE. Секрет write-only: после
// записи не возвращается ни при создании, ни списком; код создания PVE-клиента
// читает его из стора отдельно.

func (s *Server) mountProxmoxConnections() {
	s.mux.HandleFunc("GET /api/proxmox-connections", s.adminProxmoxConnections(func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.store.ListManagedConnections(r.Context())
		if err != nil {
			writeDetail(w, http.StatusInternalServerError, "managed connections unavailable")
			return
		}
		items := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			items = append(items, connectionDTO(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}))
	s.mux.HandleFunc("POST /api/proxmox-connections", s.adminProxmoxConnections(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name, Endpoint, TokenID, TokenSecret, CAPEM, Node, ResourcePool, Storage, Network string
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeDetail(w, http.StatusBadRequest, "invalid connection payload")
			return
		}
		if err := validateProxmoxConnection(body.Name, body.Endpoint, body.TokenID, body.TokenSecret, body.CAPEM, body.Node, body.ResourcePool, body.Storage, body.Network); err != nil {
			writeDetail(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		c, err := s.store.CreateManagedConnection(r.Context(), store.ManagedConnection{
			Name: body.Name, Endpoint: body.Endpoint, TokenID: body.TokenID, TokenSecret: body.TokenSecret,
			CAPEM: body.CAPEM, Node: body.Node, ResourcePool: body.ResourcePool, Storage: body.Storage, Network: body.Network,
		})
		if err != nil {
			writeDetail(w, http.StatusConflict, "connection name already exists")
			return
		}
		writeJSON(w, http.StatusCreated, connectionDTO(c))
	}))
	s.mux.HandleFunc("DELETE /api/proxmox-connections/{uid}", s.adminProxmoxConnections(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.DeleteManagedConnection(r.Context(), r.PathValue("uid")); err != nil {
			writeDetail(w, http.StatusNotFound, "connection not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *Server) adminProxmoxConnections(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := currentUser(w, r)
		if !ok {
			return
		}
		if !user.IsAdmin {
			writeDetail(w, http.StatusForbidden, "administrator access required")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") == "" {
			writeDetail(w, http.StatusForbidden, "same-origin request required")
			return
		}
		next(w, r)
	}
}

// validateProxmoxConnection применяет правила env-конфигурации к форме.
func validateProxmoxConnection(name, endpoint, tokenID, tokenSecret, caPEM, node, resourcePool, storage, network string) error {
	for field, value := range map[string]string{"name": name, "endpoint": endpoint, "tokenID": tokenID,
		"tokenSecret": tokenSecret, "node": node, "resourcePool": resourcePool, "storage": storage, "network": network} {
		if !proxmoxText(value) {
			return errors.New(field + " must be a nonempty string without controls")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(name) || len(name) > 63 {
		return errors.New("name must be a lowercase short name")
	}
	if !proxmoxOrigin(endpoint) {
		return errors.New("endpoint must be an HTTPS origin with a valid port and no credentials, path, query or fragment")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+![A-Za-z0-9_.-]+$`).MatchString(tokenID) {
		return errors.New("tokenID must have user@realm!token format")
	}
	if !proxmoxHeaderValue(tokenSecret) {
		return errors.New("tokenSecret must be a header-safe secret")
	}
	if !proxmoxCA(caPEM) {
		return errors.New("caPEM must contain only valid CA certificates in PEM format")
	}
	return nil
}

// connectionDTO — публичная форма подключения: без секрета токена.
func connectionDTO(c store.ManagedConnection) map[string]any {
	return map[string]any{"uid": c.UID, "name": c.Name, "endpoint": c.Endpoint, "tokenID": c.TokenID,
		"node": c.Node, "resourcePool": c.ResourcePool, "storage": c.Storage, "network": c.Network,
		"credentialConfigured": true}
}

// connectionItems — пулы подключений, созданных из панели: те же DTO,
// что у env-настроенных, с признаком настроенного секрета.
func (m *managedProvider) connectionItems(ctx context.Context) ([]proxmoxPoolDTO, error) {
	rows, err := m.server.store.ListManagedConnections(ctx)
	if err != nil {
		return nil, fmt.Errorf("managed connections unavailable: %w", err)
	}
	items := make([]proxmoxPoolDTO, 0, len(rows))
	for _, c := range rows {
		items = append(items, proxmoxPoolDTO{Name: c.Name, Endpoint: c.Endpoint,
			TokenID: c.TokenID, Node: c.Node, ResourcePool: c.ResourcePool,
			Storage: c.Storage, Network: c.Network, CredentialConfigured: true})
	}
	return items, nil
}
