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

const templateProfile = "claude-proxmox"
const gib int64 = 1 << 30
const mib int64 = 1 << 20

// provider drives agent containers, pools and image builds in Proxmox from
// the records of the store. admission serializes pool activation and
// connection and role changes with agent creation. lifecycle serializes
// provider operations and reconciliation within this process.
type provider struct {
	providerRuntime
	server   *Server
	registry *proxmoxRegistry
	// deliverMessage доставляет запрос адресату; обёртка для тестов.
	deliverMessage messageDeliverer
	admission      sync.Mutex
	lifecycle      sync.Mutex
	effects        actorEffectGates
	works          agentWorkCache
}

type poolUsageJSON struct {
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

func decimal(n int64) string { return strconv.FormatInt(n, 10) }

func (m *provider) pools(ctx context.Context) ([]store.Pool, error) {
	rows, err := m.server.store.ListPools(ctx)
	if err != nil {
		return nil, err
	}
	live := make([]store.Pool, 0, len(rows))
	for _, p := range rows {
		if p.State != "deleted" {
			live = append(live, p)
		}
	}
	return live, nil
}
func (m *provider) poolByName(ctx context.Context, name string) (store.Pool, error) {
	rows, err := m.pools(ctx)
	if err != nil {
		return store.Pool{}, err
	}
	for _, p := range rows {
		if p.Name == name {
			return p, nil
		}
	}
	return store.Pool{}, store.ErrNotFound
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

func (m *provider) configuredPool(name string) (store.Pool, error) {
	if m.registry == nil {
		return store.Pool{}, fmt.Errorf("%w: Proxmox runtime is not configured", app.ErrConflict)
	}
	cfg, err := m.configuredConnection(context.Background(), name)
	if err != nil {
		return store.Pool{}, err
	}
	if cfg.runtime == nil || m.registry.publicOrigin == "" || len(m.registry.relaySecret) < 32 {
		return store.Pool{}, fmt.Errorf("%w: runtime requires installed template/capacity, BROOD_PUBLIC_ORIGIN and BROOD_PROXMOX_RELAY_SECRET", app.ErrConflict)
	}
	r := cfg.runtime
	if r.diskGiB > int64(^uint64(0)>>1)/gib {
		return store.Pool{}, fmt.Errorf("%w: disk capacity exceeds int64", app.ErrInvalidCapacity)
	}
	ca := publicCAFingerprint(cfg.caPEM)
	// Метки входят в сверку poolAvailable с записанным пулом, а записанные
	// пулы приходят из фикстуры выкладки: прежний ключ ate.dev/worker-pool
	// меняется только вместе с фикстурой.
	p := store.Pool{Name: name, Connection: name, Source: store.SourceSnapshot{ClusterIdentity: "pve-" + strings.TrimPrefix(ca, "sha256:"), Endpoint: strings.TrimSuffix(cfg.endpoint, "/"), Node: cfg.node, ResourcePool: cfg.resourcePool, Storage: cfg.storage, Network: cfg.network, CAFingerprint: ca}, Labels: map[string]string{"ate.dev/worker-pool": name, "brood.dev/provider": "proxmox"}, MaxActors: r.actors, Resources: store.Resources{CPUMillicores: r.cpuMillicores, MemoryBytes: r.memoryBytes, DiskBytes: r.diskGiB * gib}, Template: store.TemplateSnapshot{Profile: templateProfile, URI: r.template, Digest: r.digest}, State: "enabled"}
	if _, err := agentResources(p); err != nil {
		return p, err
	}
	return p, nil
}

func agentResources(p store.Pool) (store.Resources, error) {
	if p.MaxActors <= 0 {
		return store.Resources{}, fmt.Errorf("%w: pool requires positive agent capacity", app.ErrInvalidCapacity)
	}
	// PVE reserves whole cores, MiB and GiB. Equal immutable shares never
	// round up beyond the installation-approved aggregate capacity.
	r := store.Resources{CPUMillicores: (p.Resources.CPUMillicores / p.MaxActors / 1000) * 1000, MemoryBytes: (p.Resources.MemoryBytes / p.MaxActors / mib) * mib, DiskBytes: (p.Resources.DiskBytes / p.MaxActors / gib) * gib}
	if r.CPUMillicores < 1000 || r.CPUMillicores > 128000 || r.MemoryBytes < 16*mib || r.MemoryBytes > 1048576*mib || r.DiskBytes < gib || r.DiskBytes > 16384*gib {
		return r, fmt.Errorf("%w: equal per-actor capacity must fit PVE core, memory and disk bounds", app.ErrInvalidCapacity)
	}
	return r, nil
}

func (m *provider) poolAvailable(p store.Pool) bool {
	configured, err := m.configuredPool(p.Connection)
	return err == nil && configured.Name == p.Name && configured.Source == p.Source && configured.Template == p.Template && configured.MaxActors == p.MaxActors && configured.Resources == p.Resources && reflect.DeepEqual(configured.Labels, p.Labels)
}

func (m *provider) poolJSON(ctx context.Context, p store.Pool) (*poolUsageJSON, error) {
	actors, err := m.server.store.ListAgents(ctx, "")
	if err != nil {
		return nil, err
	}
	var count int64
	var used store.Resources
	for _, a := range actors {
		if a.PoolUID == p.UID && a.Phase != "deleted" {
			count++
			used.CPUMillicores += a.Resources.CPUMillicores
			used.MemoryBytes += a.Resources.MemoryBytes
			used.DiskBytes += a.Resources.DiskBytes
		}
	}
	return &poolUsageJSON{UID: p.UID, State: p.State, Version: decimal(p.Revision), MaxActors: decimal(p.MaxActors), UsedActors: decimal(count), CPUMillicores: decimal(p.Resources.CPUMillicores), UsedCPUMillicores: decimal(used.CPUMillicores), MemoryBytes: decimal(p.Resources.MemoryBytes), UsedMemoryBytes: decimal(used.MemoryBytes), DiskBytes: decimal(p.Resources.DiskBytes), UsedDiskBytes: decimal(used.DiskBytes)}, nil
}

func (m *provider) inventory(ctx context.Context) ([]proxmoxPoolDTO, error) {
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
		items[i].Usage = dto
		items[i].RuntimeAvailable = m.poolAvailable(p)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func emptyBody(w http.ResponseWriter, r *http.Request) bool {
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

func (s *Server) handlePoolAction(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin {
		writeDetail(w, 403, "administrator access required")
		return
	}
	if !emptyBody(w, r) {
		return
	}
	m := s.provider
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
			pool, err = s.store.CreatePool(r.Context(), configured)
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
			pool, err = s.store.UpdatePool(r.Context(), pool, pool.Revision)
		case "delete":
			err = s.store.DeletePool(r.Context(), pool.UID, pool.Revision)
			if err == nil {
				w.WriteHeader(204)
				return
			}
		default:
			err = app.ErrNotFound
		}
	}
	if err != nil {
		providerAPIError(w, r, err)
		return
	}
	dto, err := m.poolJSON(r.Context(), pool)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, code, map[string]any{"managedPool": dto})
}

func providerAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var origin *originRefusal
	switch {
	case errors.As(err, &origin):
		writeDetail(w, 422, origin.Error())
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, 404, "agent or pool not found; it may have been deleted, reload the list")
	case errors.Is(err, proxmox.ErrPermission):
		writeDetail(w, 503, providerError(err))
	case errors.Is(err, proxmox.ErrOwnership), errors.Is(err, proxmox.ErrUncertain), errors.Is(err, proxmox.ErrTaskFailed):
		writeDetail(w, 409, providerError(err))
	case errors.Is(err, store.ErrPoolCapacity), errors.Is(err, store.ErrPoolUnavailable), errors.Is(err, store.ErrInUse), errors.Is(err, store.ErrImmutableBinding), errors.Is(err, store.ErrRevisionConflict), store.IsUniqueViolation(err):
		writeDetail(w, 409, publicDetail(err))
	default:
		apiError(w, r, err)
	}
}

func (s *Server) admitWorkload(ctx context.Context, userID, swarmID string, op app.WorkloadOperation) error {
	s.provider.admission.Lock()
	defer s.provider.admission.Unlock()
	return s.app.AdmitWorkload(ctx, userID, swarmID, op)
}

// agentProfiles — имена пулов, в которых сейчас можно создать агента:
// включённых и совпадающих с настройкой установки.
func (s *Server) agentProfiles(ctx context.Context) ([]string, error) {
	pools, err := s.provider.pools(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, p := range pools {
		if p.State == "enabled" && s.provider.poolAvailable(p) {
			out = append(out, p.Name)
		}
	}
	return out, nil
}
