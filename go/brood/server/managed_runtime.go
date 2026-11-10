package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type proxmoxAPI interface {
	NextID(context.Context) (int, error)
	Create(context.Context, proxmox.CTSpec) (string, error)
	InspectOwned(context.Context, proxmox.OwnedCT) (proxmox.CTConfig, error)
	ConfirmAbsent(context.Context, int) error
	Status(context.Context, int) (proxmox.CTStatus, error)
	WaitTask(context.Context, string) error
	Start(context.Context, proxmox.OwnedCT) (string, error)
	ConfigureBootstrap(context.Context, proxmox.OwnedCT, proxmox.BootstrapEnv) error
	Stop(context.Context, proxmox.OwnedCT) (string, error)
	Delete(context.Context, proxmox.OwnedCT) (string, error)
	Close()
}

type guestRelay interface {
	Handler() http.Handler
	HostKey() string
	Connected(string, int64) bool
	Dial(context.Context, string, int64) (net.Conn, error)
	SetMessageRouter(relay.MessageRouter)
	DialMessage(context.Context, string, int64, []byte) (net.Conn, error)
	Disconnect(string, int64)
	Close() error
}

type managedRuntime struct {
	hub       guestRelay
	newClient func(proxmox.Config) (proxmoxAPI, error)
	clients   map[string]proxmoxAPI
	publicCA  string
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	verified  sync.Map // actor UID -> incarnation verified by this server process
	readiness sync.Map // UID актора -> ожидание readiness текущей операции start
	// Швы прохода reconcile: nil и 0 значат time.Now и managedPassTimeout.
	now         func() time.Time
	passTimeout time.Duration
}

// managedPassTimeout — срок одного прохода reconcile для одного актора.
// Задача PVE может идти дольше: OCI vzcreate на живом стенде шёл 34 с.
const managedPassTimeout = 20 * time.Second

// managedTaskPendingLimit — сколько идущая задача ждёт без диагностики.
// Отсчёт от UpdatedAt актора: привязка UPID его обновляет.
const managedTaskPendingLimit = 15 * time.Minute

const managedTaskPendingError = "Proxmox task has not finished; allocation retained"

// errManagedTaskPending — задача ещё идёт; истёк только срок прохода.
var errManagedTaskPending = errors.New("managed Proxmox task is still running")

func (m *managedRuntime) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

func (m *managedRuntime) actorPassTimeout() time.Duration {
	if m.passTimeout <= 0 {
		return managedPassTimeout
	}
	return m.passTimeout
}

// managedTaskPending отличает идущую задачу от исхода: WaitTask прервал только
// истёкший срок этого прохода. Таймаут HTTP-клиента, отмена, сбой сети и отказ
// PVE не делают задачу «ещё идущей».
func managedTaskPending(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// pendingQuiet сообщает, что идущую задачу пока не показывают ошибкой: предел
// ожидания не истёк или диагностика уже записана. Нечитаемая отметка времени
// не продлевает ожидание.
func (m *managedRuntime) pendingQuiet(a store.ManagedActor) bool {
	if a.Phase == "error" && a.Error == managedTaskPendingError {
		return true
	}
	since, err := time.Parse(time.RFC3339Nano, a.UpdatedAt)
	return err == nil && m.clock().Sub(since) < managedTaskPendingLimit
}

func (m *managedProvider) initialize(cfg Config) error {
	rows, err := m.server.store.ListManagedConnections(context.Background())
	if err != nil {
		return errors.New("managed connections unavailable")
	}
	for _, c := range rows {
		if m.registry != nil {
			if _, exists := m.registry.pools[c.Name]; exists {
				return errors.New("managed connection name conflicts with environment configuration")
			}
		}
	}
	m.clients = map[string]proxmoxAPI{}
	m.newClient = func(c proxmox.Config) (proxmoxAPI, error) { return proxmox.NewClient(c) }
	if m.registry == nil || len(m.registry.relaySecret) < 32 {
		return nil
	}
	hub, err := relay.NewHub([]byte(m.registry.relaySecret), m.authorizeGuest)
	if err != nil {
		return err
	}
	m.hub = hub
	m.enableSwarmMessages()
	if cfg.TLSCert != "" {
		data, err := os.ReadFile(cfg.TLSCert)
		if err != nil {
			hub.Close()
			return proxmoxConfigError("BROOD_TLS_CERT", "cannot read public relay TLS certificate")
		}
		m.publicCA = string(data)
	}
	return nil
}
func (m *managedProvider) start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		m.reconcileOnce(ctx)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.reconcileOnce(ctx)
			}
		}
	}()
}
func (m *managedProvider) stopReconcile() {
	if m.cancel != nil {
		m.cancel()
		m.workers.Wait()
	}
}
func (m *managedProvider) close() {
	m.stopReconcile()
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if m.hub != nil {
		m.hub.Close()
	}
	for _, client := range m.clients {
		client.Close()
	}
}

func guestToken(secret, uid string, generation int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("brood/proxmox/guest/v1\x00" + uid + "\x00" + managedDecimal(generation)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (m *managedProvider) authorizeGuest(uid string, generation int64, token string) bool {
	if m.registry == nil || len(m.registry.relaySecret) < 32 || generation <= 0 {
		return false
	}
	expected := guestToken(m.registry.relaySecret, uid, generation)
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a, err := m.server.store.GetManagedActor(ctx, uid)
	// Only a start boots the guest. Create leaves the CT stopped, and a reset
	// keeps the old incarnation until cleanup, even after a failed stage settles.
	return err == nil && a.Phase != "deleted" && a.Desired == "running" && a.Operation == "start" && a.Incarnation == generation
}

func managedOwned(a store.ManagedActor) proxmox.OwnedCT {
	return proxmox.OwnedCT{VMID: a.CTID, Owner: proxmox.Ownership{PoolUID: a.PoolUID, ActorUID: a.UID, Incarnation: uint64(a.Incarnation), Nonce: a.OwnerNonce}}
}

func (m *managedProvider) client(p store.ManagedPool) (proxmoxAPI, error) {
	if !m.poolAvailable(p) {
		return nil, fmt.Errorf("%w: activated Proxmox configuration is unavailable or changed", app.ErrConflict)
	}
	if client := m.clients[p.UID]; client != nil {
		return client, nil
	}
	cfg, err := m.configuredConnection(context.Background(), p.Connection)
	if err != nil {
		return nil, err
	}
	secret := m.registry.tokens[cfg.credentialRef]
	if cfg.credentialRef == "" {
		secret, err = m.server.store.ManagedConnectionSecret(context.Background(), p.Connection)
		if err != nil {
			return nil, fmt.Errorf("%w: managed credential unavailable", app.ErrConflict)
		}
	}
	templateStorage, _, _ := strings.Cut(p.Template.URI, ":vztmpl/")
	client, err := m.newClient(proxmox.Config{Endpoint: cfg.endpoint, TokenID: cfg.tokenID, TokenSecret: secret, CAPEM: cfg.caPEM, Node: cfg.node, ResourcePool: cfg.resourcePool, Storage: cfg.storage, Network: cfg.network, TemplateStorage: templateStorage, TemplatePrefix: "brood-claude-proxmox-"})
	if err != nil {
		return nil, fmt.Errorf("%w: invalid installed PVE client configuration", app.ErrConflict)
	}
	m.clients[p.UID] = client
	return client, nil
}

// managedGuestHosts is the immutable public egress policy of claude-proxmox.
// Exact names only: actor input and the installation's private origin never
// expand this list. Guest policy independently rejects private DNS answers.
const managedGuestHosts = `["api.anthropic.com","claude.ai","console.anthropic.com","platform.claude.com","downloads.claude.ai","github.com","api.github.com","codeload.github.com","raw.githubusercontent.com","objects.githubusercontent.com","release-assets.githubusercontent.com","gitlab.com","registry.npmjs.org","npmjs.org","www.npmjs.com","nodejs.org","bun.sh","bun.com","pypi.org","files.pythonhosted.org","proxy.golang.org","sum.golang.org","storage.googleapis.com","go.dev","dl.google.com","crates.io","index.crates.io","static.crates.io","static.rust-lang.org","sh.rustup.rs","deb.debian.org","security.debian.org","archive.ubuntu.com","security.ubuntu.com"]`

func (m *managedProvider) createSpec(a store.ManagedActor) proxmox.CTSpec {
	origin := strings.TrimSuffix(m.registry.publicOrigin, "/")
	return proxmox.CTSpec{VMID: a.CTID, Hostname: "brood-" + a.UID, Template: a.Template.URI, Owner: managedOwned(a).Owner, Cores: int(a.Resources.CPUMillicores / 1000), MemoryMiB: int(a.Resources.MemoryBytes / mib), DiskGiB: int(a.Resources.DiskBytes / gib), Env: proxmox.BootstrapEnv{"BROOD_GUEST_ACTOR_UID": a.UID, "BROOD_GUEST_GENERATION": managedDecimal(a.Incarnation), "BROOD_GUEST_RELAY_URL": "wss" + strings.TrimPrefix(origin, "https") + "/api/proxmox/guest/" + a.UID + "/" + managedDecimal(a.Incarnation), "BROOD_GUEST_RELAY_TOKEN": guestToken(m.registry.relaySecret, a.UID, a.Incarnation), "BROOD_GUEST_CA_PEM_B64": base64.StdEncoding.EncodeToString([]byte(m.publicCA)), "BROOD_GUEST_ALLOWED_HOSTS": managedGuestHosts, "BROOD_GUEST_RELAY_HOST_KEY": strings.TrimSpace(m.hub.HostKey())}}
}

func (m *managedProvider) saveObservation(ctx context.Context, a store.ManagedActor, phase, upid, detail string) (store.ManagedActor, error) {
	a.Phase = phase
	a.Error = detail
	if upid != "" {
		a.UPID = upid
	}
	return m.server.store.UpdateManagedActor(ctx, a, a.Revision)
}
func (m *managedProvider) recordTask(ctx context.Context, a store.ManagedActor, upid string, err error) (store.ManagedActor, error) {
	// Provider evidence must survive the browser disconnecting after submission.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if upid != "" {
		var bindErr error
		a, bindErr = m.server.store.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, upid)
		if bindErr != nil {
			return a, bindErr
		}
	}
	if err != nil {
		return m.saveObservation(ctx, a, "error", "", managedProviderError(err))
	}
	return a, nil
}
func managedProviderError(err error) string {
	switch {
	case errors.Is(err, proxmox.ErrOwnership):
		return "container ownership or approved configuration does not match; allocation retained"
	case errors.Is(err, proxmox.ErrPermission):
		return "Proxmox permission denied; allocation retained"
	case errors.Is(err, proxmox.ErrTaskFailed):
		return "Proxmox task failed; allocation retained"
	case errors.Is(err, errManagedTaskPending):
		return managedTaskPendingError
	case errors.Is(err, proxmox.ErrUncertain):
		return "Proxmox operation outcome is uncertain; recorded CTID and reservation retained"
	default:
		return "Proxmox reconciliation unavailable; allocation retained"
	}
}
func inspectManaged(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (proxmox.CTConfig, error) {
	return client.InspectOwned(ctx, managedOwned(a))
}

func managedObservedState(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (string, error) {
	if _, err := inspectManaged(ctx, client, a); err != nil {
		// Scoped PVE read может вернуть 403/500 даже после удаления. Только
		// положительный global proof заменяет исходный отказ чтения.
		if client.ConfirmAbsent(ctx, a.CTID) == nil {
			return "absent", nil
		}
		return "", err
	}
	status, err := client.Status(ctx, a.CTID)
	if err != nil {
		return "", err
	}
	if status.Status != "running" && status.Status != "stopped" {
		return "", proxmox.ErrUncertain
	}
	return status.Status, nil
}

func (m *managedProvider) launch(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	var upid string
	var err error
	switch a.Stage {
	case "create":
		upid, err = client.Create(ctx, m.createSpec(a))
	case "start":
		err = client.ConfigureBootstrap(ctx, managedOwned(a), m.createSpec(a).Env)
		if err == nil {
			upid, err = client.Start(ctx, managedOwned(a))
		}
	case "stop":
		upid, err = client.Stop(ctx, managedOwned(a))
	case "delete":
		upid, err = client.Delete(ctx, managedOwned(a))
	default:
		return a, fmt.Errorf("%w: unsupported managed intent", app.ErrConflict)
	}
	// An API error is durable state, not permission to retry a write or pick a new CTID.
	return m.recordTask(ctx, a, upid, err)
}

func (m *managedProvider) reconcileOnce(parent context.Context) {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	actors, err := m.server.store.ListManagedActors(parent, "")
	if err != nil {
		return
	}
	for _, a := range actors {
		if parent.Err() != nil {
			return
		}
		if a.Phase == "deleted" {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, m.actorPassTimeout())
		_, err = m.reconcileActor(ctx, a)
		cancel()
		if err == nil || parent.Err() != nil {
			continue
		}
		current, getErr := m.server.store.GetManagedActor(parent, a.UID)
		if getErr != nil || current.Revision != a.Revision || current.Phase == "deleted" {
			continue
		}
		// Идущая задача — не исход: стадия, UPID и ревизия остаются,
		// следующий проход продолжит ждать тот же UPID.
		if errors.Is(err, errManagedTaskPending) && m.pendingQuiet(current) {
			continue
		}
		m.saveObservation(parent, current, "error", "", managedProviderError(err))
	}
}

func (m *managedProvider) reconcileActor(ctx context.Context, a store.ManagedActor) (result store.ManagedActor, err error) {
	defer func() {
		if !errors.Is(err, store.ErrRevisionConflict) && (err != nil || result.Phase != "running") {
			m.verified.Delete(a.UID)
		}
	}()
	current, err := m.server.store.GetManagedActor(ctx, a.UID)
	if err != nil {
		return a, err
	}
	if current.Revision != a.Revision {
		return a, store.ErrRevisionConflict
	}
	// Согласованный терминальный отказ требует нового явного намерения.
	// Диагностика сохраняется даже при недоступной конфигурации провайдера.
	if a.Stage == "done" && a.Phase == "error" && a.Error == managedProviderError(proxmox.ErrTaskFailed) {
		return a, nil
	}
	p, err := m.server.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		return a, err
	}
	client, err := m.client(p)
	if err != nil {
		return a, err
	}
	if a.SourceFingerprint != p.SourceFingerprint || a.Template != p.Template {
		return a, proxmox.ErrOwnership
	}
	// A missing task response is not proof that no task is still running. Never
	// replay a submitted stage, even if the CT is currently stopped or absent.
	if a.Stage != "done" && a.UPID == "" {
		return a, proxmox.ErrUncertain
	}
	if a.Stage == "reconcile" && a.Operation == "reset" {
		return a, fmt.Errorf("%w: legacy reset requires manual ownership reconciliation; allocation retained", app.ErrConflict)
	}
	// Done фиксирует согласование task, включая отказ. Сохранённый UPID —
	// доказательство, а не повод читать заново уже истёкшие логи PVE.
	if a.Stage != "done" && a.UPID != "" {
		if err := client.WaitTask(ctx, a.UPID); err != nil {
			if errors.Is(err, proxmox.ErrTaskFailed) {
				return m.resolveTaskFailure(ctx, client, a)
			}
			// Срок прохода короче долгих задач PVE: это ожидание, а не отказ.
			// Ни повторной записи, ни замены UPID.
			if managedTaskPending(ctx, err) {
				return a, errManagedTaskPending
			}
			return a, err
		}
	}
	if a.Stage == "delete" {
		return m.finishCleanup(ctx, client, a)
	}
	observed, err := managedObservedState(ctx, client, a)
	if err != nil {
		return a, err
	}
	stage := a.Stage
	if stage == "done" || stage == "reconcile" {
		stage = a.Operation
		if a.Operation == "reset" && a.Stage == "done" {
			stage = "create"
		}
	}
	switch stage {
	case "create":
		if observed != "stopped" {
			return a, proxmox.ErrUncertain
		}
		if a.Stage != "done" {
			a, err = m.server.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
		} else if a.Phase != "stopped" || a.Error != "" {
			a, err = m.saveObservation(ctx, a, "stopped", "", "")
		}
		if err != nil {
			return a, err
		}
		a, err = m.server.store.BeginManagedActorOperation(ctx, a.UID, a.Revision, store.NewID(), "start")
		if err != nil {
			return a, err
		}
		return m.launch(ctx, client, a)
	case "stop", "delete":
		if observed == "running" {
			return a, proxmox.ErrUncertain
		}
		if a.Stage == "done" {
			if a.Phase == observed && a.Error == "" {
				return a, nil
			}
			if observed == "absent" {
				return a, proxmox.ErrNotFound
			}
			return m.saveObservation(ctx, a, observed, "", "")
		}
		a, err = m.server.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
		if err != nil || a.Stage == "done" {
			return a, err
		}
		if observed == "absent" {
			return m.finishConfirmedCleanup(ctx, client, a)
		}
		return m.launch(ctx, client, a)
	case "start":
		if observed != "running" {
			return a, proxmox.ErrUncertain
		}
		if a.Stage != "done" {
			// Завершение task не зависит от readiness гостя. Фиксируем task fence
			// без публикации running до настоящего ttyd probe.
			err = m.server.store.WithTx(ctx, func(tx context.Context) error {
				var e error
				a, e = m.server.store.ConfirmManagedActorStage(tx, a.UID, a.Revision, a.OperationID, a.Stage, "running")
				if e == nil {
					a, e = m.saveObservation(tx, a, "provisioning", "", "")
				}
				return e
			})
			if err != nil {
				return a, err
			}
		}
		if m.hub == nil || !m.hub.Connected(a.UID, a.Incarnation) || !m.probe(ctx, a) {
			return m.waitReadiness(ctx, a)
		}
		m.readiness.Delete(a.UID)
		if a.Phase != "running" || a.Error != "" {
			a, err = m.saveObservation(ctx, a, "running", "", "")
		}
		if err != nil {
			return a, err
		}
		m.verified.Store(a.UID, a.Incarnation)
		return a, nil
	}
	return a, fmt.Errorf("%w: unsupported managed reconciliation intent", app.ErrConflict)
}

// resolveTaskFailure is called only after WaitTask returned ErrTaskFailed for
// this exact bound slot. Observation alone never proves task terminality.
func (m *managedProvider) resolveTaskFailure(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	observed, err := managedObservedState(ctx, client, a)
	if err != nil {
		return a, err
	}
	err = m.server.store.WithTx(ctx, func(tx context.Context) error {
		var e error
		a, e = m.saveObservation(tx, a, "error", "", managedProviderError(proxmox.ErrTaskFailed))
		if e == nil {
			a, e = m.server.store.ResolveManagedActorTaskFailure(tx, a.UID, a.Revision, a.OperationID, a.Stage, a.UPID, observed)
		}
		return e
	})
	return a, err
}

type managedReadiness struct {
	operationID string
	since       time.Time
}

const managedReadinessError = "guest terminal is not ready; use stop/resume to recover"

func (m *managedProvider) waitReadiness(ctx context.Context, a store.ManagedActor) (store.ManagedActor, error) {
	value, _ := m.readiness.LoadOrStore(a.UID, managedReadiness{a.OperationID, time.Now()})
	waiting := value.(managedReadiness)
	if waiting.operationID != a.OperationID {
		waiting = managedReadiness{a.OperationID, time.Now()}
		m.readiness.Store(a.UID, waiting)
	}
	phase, detail := "provisioning", ""
	if time.Since(waiting.since) >= 2*time.Minute {
		phase, detail = "error", managedReadinessError
	}
	if a.Phase == phase && a.Error == detail {
		return a, nil
	}
	return m.saveObservation(ctx, a, phase, "", detail)
}

func (m *managedProvider) finishCleanup(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	// Config 404 and ACL-filtered inventory are not global absence evidence.
	if err := client.ConfirmAbsent(ctx, a.CTID); err != nil {
		return a, err
	}
	return m.finishConfirmedCleanup(ctx, client, a)
}

// finishConfirmedCleanup consumes an already-positive global absence proof.
func (m *managedProvider) finishConfirmedCleanup(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	if a.Operation == "delete" {
		return m.server.store.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID)
	}
	if a.Operation != "reset" {
		return a, app.ErrConflict
	}
	a, err := m.server.store.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, store.NewID())
	if err != nil {
		return a, err
	}
	return m.launch(ctx, client, a)
}

func (m *managedProvider) probe(ctx context.Context, a store.ManagedActor) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := m.hub.Dial(ctx, a.UID, a.Incarnation)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	req, _ := http.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	if req.Write(conn) != nil {
		return false
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == 200
}

func (m *managedProvider) actorByName(ctx context.Context, swarm, name string) (store.ManagedActor, error) {
	actors, err := m.server.store.ListManagedActors(ctx, swarm)
	if err != nil {
		return store.ManagedActor{}, err
	}
	for _, a := range actors {
		if a.Name == name && a.Phase != "deleted" {
			return a, nil
		}
	}
	return store.ManagedActor{}, store.ErrNotFound
}

// managedActorJSON renders a managed actor; originMode is the caller's current
// recomputation for a.Origin ("" when the origin has no admissible mode).
func managedActorJSON(a store.ManagedActor, p store.ManagedPool, atespace, originMode string) map[string]any {
	state := "ACTOR_STATE_UNSPECIFIED"
	switch a.Phase {
	case "provisioning":
		state = "ACTOR_STATE_RESUMING"
	case "running":
		state = "ACTOR_STATE_RUNNING"
	case "stopping":
		state = "ACTOR_STATE_SUSPENDING"
	case "deleting":
		state = "ACTOR_STATE_DELETING"
	case "error":
		state = "ACTOR_STATE_CRASHED"
	}
	return map[string]any{"metadata": map[string]any{"name": a.Name, "uid": a.UID, "version": managedDecimal(a.Revision), "atespace": atespace}, "actorTemplate": map[string]any{"name": managedTemplatePrefix + p.Name, "atespace": atespace}, "provider": "proxmox", "role": a.Role, "profile": a.Template.Profile, "generation": managedDecimal(a.Incarnation), "phase": a.Phase, "desired": a.Desired, "placement": map[string]any{"node": p.Source.Node, "ctid": a.CTID, "pool": p.Name}, "capabilities": managedCapabilities(), "templateDefinition": map[string]any{"profile": a.Template.Profile, "digest": a.Template.Digest, "resources": map[string]string{"cpuMillicores": managedDecimal(a.Resources.CPUMillicores), "memoryBytes": managedDecimal(a.Resources.MemoryBytes), "diskBytes": managedDecimal(a.Resources.DiskBytes)}}, "status": map[string]any{"state": state, "errorMessage": a.Error}, "origin": a.Origin, "originMode": originMode}
}

func (m *managedProvider) actorsJSON(ctx context.Context, sw app.Swarm, nativeNames map[string]bool) ([]any, error) {
	actors, err := m.server.store.ListManagedActors(ctx, sw.ID)
	if err != nil {
		return nil, err
	}
	access, err := m.server.projectGitAccess(ctx, sw.ProjectID)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, a := range actors {
		if a.Phase == "deleted" {
			continue
		}
		if a.AtespaceUID != sw.AtespaceUID {
			return nil, app.ErrConflict
		}
		if nativeNames[a.Name] {
			return nil, fmt.Errorf("%w: native actor conflicts with managed allocation", app.ErrConflict)
		}
		p, err := m.server.store.GetManagedPool(ctx, a.PoolUID)
		if err != nil {
			return nil, err
		}
		out = append(out, managedActorJSON(a, p, sw.AtespaceName, access.mode(a.Origin)))
	}
	return out, nil
}

func managedGeneration(host string) (string, int64, bool) {
	name, port, err := net.SplitHostPort(host)
	if err != nil || port != "8080" {
		return "", 0, false
	}
	value, ok := strings.CutSuffix(name, ".proxmox.invalid")
	if !ok {
		return "", 0, false
	}
	uid, g, ok := strings.Cut(value, ".")
	generation, err := strconv.ParseInt(g, 10, 64)
	return uid, generation, ok && err == nil && generation > 0 && viewSegment(uid)
}
