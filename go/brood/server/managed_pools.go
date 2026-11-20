package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

const managedProfile = "claude-proxmox"
const managedTemplatePrefix = "claude-proxmox-"
const gib int64 = 1 << 30
const mib int64 = 1 << 20

// managedProvider owns Brood inventory, never native workers or their identities.
// admission serializes profile activation with both native publication paths.
// lifecycle serializes provider operations and reconciliation within this process.
type managedProvider struct {
	managedRuntime
	server   *Server
	registry *proxmoxRegistry
	// deliverMessage доставляет запрос адресату; обёртка для тестов.
	deliverMessage messageDeliverer
	admission      sync.Mutex
	lifecycle      sync.Mutex
	effects        actorEffectGates
}

type managedPoolJSON struct {
	UID               string `json:"uid"`
	State             string `json:"state"`
	Version           string `json:"version"`
	MaxActors         string `json:"maxActors"`
	UsedActors        string `json:"usedActors"`
	CPUMillicores     string `json:"cpuMillicores"`
	UsedCPUMillicores string `json:"usedCPUMillicores"`
	MemoryBytes       string `json:"memoryBytes"`
	UsedMemoryBytes   string `json:"usedMemoryBytes"`
	DiskBytes         string `json:"diskBytes"`
	UsedDiskBytes     string `json:"usedDiskBytes"`
}

func managedDecimal(n int64) string { return strconv.FormatInt(n, 10) }

func (m *managedProvider) pools(ctx context.Context) ([]store.ManagedPool, error) {
	rows, err := m.server.store.ListManagedPools(ctx)
	if err != nil {
		return nil, err
	}
	live := make([]store.ManagedPool, 0, len(rows))
	for _, p := range rows {
		if p.State != "deleted" {
			live = append(live, p)
		}
	}
	return live, nil
}
func (m *managedProvider) poolByName(ctx context.Context, name string) (store.ManagedPool, error) {
	rows, err := m.pools(ctx)
	if err != nil {
		return store.ManagedPool{}, err
	}
	for _, p := range rows {
		if p.Name == name {
			return p, nil
		}
	}
	return store.ManagedPool{}, store.ErrNotFound
}

func publicCAFingerprint(ca string) string {
	var canonical []byte
	for {
		b, rest := pem.Decode([]byte(ca))
		if b == nil {
			break
		}
		canonical = append(canonical, b.Bytes...)
		ca = string(rest)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (m *managedProvider) configuredPool(name string) (store.ManagedPool, error) {
	if m.registry == nil {
		return store.ManagedPool{}, fmt.Errorf("%w: Proxmox runtime is not configured", app.ErrConflict)
	}
	cfg, err := m.configuredConnection(context.Background(), name)
	if err != nil {
		return store.ManagedPool{}, err
	}
	if cfg.runtime == nil || m.registry.publicOrigin == "" || len(m.registry.relaySecret) < 32 {
		return store.ManagedPool{}, fmt.Errorf("%w: runtime requires installed template/capacity, BROOD_PUBLIC_ORIGIN and BROOD_PROXMOX_RELAY_SECRET", app.ErrConflict)
	}
	r := cfg.runtime
	if r.diskGiB > int64(^uint64(0)>>1)/gib {
		return store.ManagedPool{}, fmt.Errorf("%w: disk capacity exceeds int64", app.ErrInvalidCapacity)
	}
	ca := publicCAFingerprint(cfg.caPEM)
	p := store.ManagedPool{Name: name, Connection: name, Source: store.ManagedSourceSnapshot{ClusterIdentity: "pve-" + strings.TrimPrefix(ca, "sha256:"), Endpoint: strings.TrimSuffix(cfg.endpoint, "/"), Node: cfg.node, ResourcePool: cfg.resourcePool, Storage: cfg.storage, Network: cfg.network, CAFingerprint: ca}, Labels: map[string]string{"ate.dev/worker-pool": name, "brood.dev/provider": "proxmox"}, MaxActors: r.actors, Resources: store.ManagedResources{CPUMillicores: r.cpuMillicores, MemoryBytes: r.memoryBytes, DiskBytes: r.diskGiB * gib}, Template: store.ManagedTemplateSnapshot{Profile: managedProfile, URI: r.template, Digest: r.digest}, State: "enabled"}
	if _, err := managedActorResources(p); err != nil {
		return p, err
	}
	return p, nil
}

func managedActorResources(p store.ManagedPool) (store.ManagedResources, error) {
	if p.MaxActors <= 0 {
		return store.ManagedResources{}, fmt.Errorf("%w: managed pool requires positive actor capacity", app.ErrInvalidCapacity)
	}
	// PVE reserves whole cores, MiB and GiB. Equal immutable shares never
	// round up beyond the installation-approved aggregate capacity.
	r := store.ManagedResources{CPUMillicores: (p.Resources.CPUMillicores / p.MaxActors / 1000) * 1000, MemoryBytes: (p.Resources.MemoryBytes / p.MaxActors / mib) * mib, DiskBytes: (p.Resources.DiskBytes / p.MaxActors / gib) * gib}
	if r.CPUMillicores < 1000 || r.CPUMillicores > 128000 || r.MemoryBytes < 16*mib || r.MemoryBytes > 1048576*mib || r.DiskBytes < gib || r.DiskBytes > 16384*gib {
		return r, fmt.Errorf("%w: equal per-actor capacity must fit PVE core, memory and disk bounds", app.ErrInvalidCapacity)
	}
	return r, nil
}

func (m *managedProvider) poolAvailable(p store.ManagedPool) bool {
	configured, err := m.configuredPool(p.Connection)
	return err == nil && configured.Name == p.Name && configured.Source == p.Source && configured.Template == p.Template && configured.MaxActors == p.MaxActors && configured.Resources == p.Resources && reflect.DeepEqual(configured.Labels, p.Labels)
}

func (m *managedProvider) poolJSON(ctx context.Context, p store.ManagedPool) (*managedPoolJSON, error) {
	actors, err := m.server.store.ListManagedActors(ctx, "")
	if err != nil {
		return nil, err
	}
	var count int64
	var used store.ManagedResources
	for _, a := range actors {
		if a.PoolUID == p.UID && a.Phase != "deleted" {
			count++
			used.CPUMillicores += a.Resources.CPUMillicores
			used.MemoryBytes += a.Resources.MemoryBytes
			used.DiskBytes += a.Resources.DiskBytes
		}
	}
	return &managedPoolJSON{UID: p.UID, State: p.State, Version: managedDecimal(p.Revision), MaxActors: managedDecimal(p.MaxActors), UsedActors: managedDecimal(count), CPUMillicores: managedDecimal(p.Resources.CPUMillicores), UsedCPUMillicores: managedDecimal(used.CPUMillicores), MemoryBytes: managedDecimal(p.Resources.MemoryBytes), UsedMemoryBytes: managedDecimal(used.MemoryBytes), DiskBytes: managedDecimal(p.Resources.DiskBytes), UsedDiskBytes: managedDecimal(used.DiskBytes)}, nil
}

func (m *managedProvider) inventory(ctx context.Context) ([]proxmoxPoolDTO, error) {
	items := m.registry.items()
	// Подключения панели и env используют один инвентарь и путь активации.
	dbItems, err := m.connectionItems(ctx)
	if err != nil {
		return nil, err
	}
	items = append(items, dbItems...)
	positions := map[string]int{}
	for i, item := range items {
		positions[item.Name] = i
	}
	pools, err := m.pools(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range pools {
		dto, err := m.poolJSON(ctx, p)
		if err != nil {
			return nil, err
		}
		i, ok := positions[p.Name]
		if !ok {
			i = len(items)
			items = append(items, proxmoxPoolDTO{Name: p.Name, Endpoint: p.Source.Endpoint, Node: p.Source.Node, ResourcePool: p.Source.ResourcePool, Storage: p.Source.Storage, Network: p.Source.Network})
		}
		items[i].ManagedPool = dto
		items[i].RuntimeAvailable = m.poolAvailable(p)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func emptyManagedBody(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeDetail(w, 400, "empty request body required")
		return false
	}
	if strings.TrimSpace(string(body)) == "" {
		return true
	}
	obj, err := proxmoxObject(string(body))
	if err != nil || len(obj) != 0 {
		writeDetail(w, 400, "empty request body required; runtime configuration is installation-owned")
		return false
	}
	return true
}

func (s *Server) handleManagedPool(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, 403, "administrator access required")
		return
	}
	if !emptyManagedBody(w, r) {
		return
	}
	m := s.managed
	m.admission.Lock()
	defer m.admission.Unlock()
	name := r.PathValue("name")
	action := r.PathValue("action")
	pool, err := m.poolByName(r.Context(), name)
	code := 200
	if action == "activate" {
		configured, configErr := m.configuredPool(name)
		if configErr != nil {
			apiError(w, r, configErr)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			configured.UID = store.NewID()
			pool, err = s.store.CreateManagedPool(r.Context(), configured)
			code = 201
		} else if err == nil && !m.poolAvailable(pool) {
			err = fmt.Errorf("%w: activated pool differs from installation configuration", app.ErrConflict)
		}
	} else if err == nil {
		switch action {
		case "drain", "enable":
			if action == "enable" && !m.poolAvailable(pool) {
				err = fmt.Errorf("%w: installed pool configuration is unavailable or changed", app.ErrConflict)
				break
			}
			pool.State = "draining"
			if action == "enable" {
				pool.State = "enabled"
			}
			pool, err = s.store.UpdateManagedPool(r.Context(), pool, pool.Revision)
		case "delete":
			err = s.store.DeleteManagedPool(r.Context(), pool.UID, pool.Revision)
			if err == nil {
				w.WriteHeader(204)
				return
			}
		default:
			err = app.ErrNotFound
		}
	}
	if err != nil {
		managedAPIError(w, r, err)
		return
	}
	dto, err := m.poolJSON(r.Context(), pool)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, code, map[string]any{"managedPool": dto})
}

func managedAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var origin *originRefusal
	switch {
	case errors.As(err, &origin):
		writeDetail(w, 422, origin.Error())
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, 404, "managed resource not found")
	case errors.Is(err, proxmox.ErrPermission):
		writeDetail(w, 503, managedProviderError(err))
	case errors.Is(err, proxmox.ErrOwnership), errors.Is(err, proxmox.ErrUncertain), errors.Is(err, proxmox.ErrTaskFailed):
		writeDetail(w, 409, managedProviderError(err))
	case errors.Is(err, store.ErrPoolCapacity), errors.Is(err, store.ErrPoolUnavailable), errors.Is(err, store.ErrManagedChildren), errors.Is(err, store.ErrManagedBinding), errors.Is(err, store.ErrRevisionConflict), store.IsUniqueViolation(err):
		writeDetail(w, 409, err.Error())
	default:
		apiError(w, r, err)
	}
}

func (g *Gateway) CheckSwarmDeletion(id string) error {
	has, err := g.store.HasManagedActorsBySwarm(context.Background(), id)
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("%w: managed actor allocations or uncertain cleanup remain", app.ErrSwarmNotEmpty)
	}
	return nil
}

func (s *Server) admitWorkload(userID, swarmID string, op app.WorkloadOperation) error {
	s.managed.admission.Lock()
	defer s.managed.admission.Unlock()
	return s.app.AdmitWorkload(userID, swarmID, op)
}

func managedTemplateJSON(pool store.ManagedPool) map[string]any {
	resources, _ := managedActorResources(pool)
	return map[string]any{"metadata": map[string]any{"name": managedTemplatePrefix + pool.Name, "uid": managedTemplatePrefix + pool.UID, "version": managedDecimal(pool.Revision)}, "provider": "proxmox", "profile": managedProfile, "readOnly": true, "capabilities": managedCapabilities(), "resources": map[string]any{"limits": map[string]string{"cpuMillicores": managedDecimal(resources.CPUMillicores), "memoryBytes": managedDecimal(resources.MemoryBytes)}}}
}
func managedCapabilities() map[string]bool {
	return map[string]bool{"workspace": true, "terminal": true, "memoryCheckpoint": false}
}

func (m *managedProvider) templatePool(ctx context.Context, name string) (store.ManagedPool, error) {
	if !strings.HasPrefix(name, managedTemplatePrefix) {
		return store.ManagedPool{}, store.ErrNotFound
	}
	return m.poolByName(ctx, strings.TrimPrefix(name, managedTemplatePrefix))
}

func (s *Server) managedTemplates(ctx context.Context) ([]any, error) {
	pools, err := s.managed.pools(ctx)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, p := range pools {
		if p.State == "enabled" && s.managed.poolAvailable(p) {
			out = append(out, managedTemplateJSON(p))
		}
	}
	return out, nil
}
