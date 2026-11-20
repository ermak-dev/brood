package server

import (
	"brood.dev/go/brood/proxmox/sshboot"
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
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

// proxmoxAPI is the PVE client of the managed provider (proxmox.Client). A
// write that fails without a UPID and without wrapping proxmox.ErrUncertain
// submitted nothing: it failed on a read before the write (ownership, digest,
// status) or PVE refused it explicitly (401, 403, 409). Any other failure of a
// write may have been applied and wraps proxmox.ErrUncertain.
type proxmoxAPI interface {
	NextID(context.Context) (int, error)
	Create(context.Context, proxmox.CTSpec) (string, error)
	InspectOwned(context.Context, proxmox.OwnedCT) (proxmox.CTConfig, error)
	ConfirmAbsent(context.Context, int) error
	Status(context.Context, int) (proxmox.CTStatus, error)
	WaitTask(context.Context, string) error
	Start(context.Context, proxmox.OwnedCT) (string, error)
	Addresses(context.Context, int) ([]netip.Addr, error)
	Clone(context.Context, proxmox.OwnedCT, proxmox.CTSpec) (string, error)
	Resize(context.Context, proxmox.OwnedCT, int, int) error
	ConvertToTemplate(context.Context, proxmox.OwnedCT) error
	Stop(context.Context, proxmox.OwnedCT) (string, error)
	Shutdown(context.Context, proxmox.OwnedCT) (string, error)
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
	// identity — SSH-ключ установки для входа в контейнеры; agentInstalled —
	// UID воркера -> операция, в которой агент поставлен этим процессом;
	// agentHostKeys — ключ хоста контейнера, принятый при первом входе.
	identity       *sshboot.Identity
	agentInstalled sync.Map
	// buildContext — время жизни сборок образов; его отменяет остановка
	// сервера.
	buildContext  context.Context
	agentHostKeys sync.Map
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
	identity, err := loadAgentIdentity(cfg.DBPath)
	if err != nil {
		return errors.New("installation SSH identity is unavailable: " + err.Error())
	}
	m.identity = identity
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
	m.buildContext = ctx
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
	client, err := m.newClient(proxmox.Config{Endpoint: cfg.endpoint, TokenID: cfg.tokenID, TokenSecret: secret, CAPEM: cfg.caPEM, Node: cfg.node, ResourcePool: cfg.resourcePool, Storage: cfg.storage, Network: cfg.network, TemplateStorage: templateStorage, TemplatePrefix: "brood-", BaseTemplate: p.Template.URI})
	if err != nil {
		return nil, fmt.Errorf("%w: invalid installed PVE client configuration", app.ErrConflict)
	}
	m.clients[p.UID] = client
	return client, nil
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
	case errors.Is(err, errAgentWaiting):
		return "Proxmox did not confirm the bootstrap configuration of the stopped container; it is written again before the start"
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
	_, observed, err := managedObservation(ctx, client, a)
	return observed, err
}

// managedObservation — managedObservedState вместе с проверенной владением
// конфигурацией CT; у отсутствующей CT конфигурация пуста.
func managedObservation(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (proxmox.CTConfig, string, error) {
	config, err := inspectManaged(ctx, client, a)
	if err != nil {
		// Scoped PVE read может вернуть 403/500 даже после удаления. Только
		// положительный global proof заменяет исходный отказ чтения.
		if client.ConfirmAbsent(ctx, a.CTID) == nil {
			return proxmox.CTConfig{}, "absent", nil
		}
		return proxmox.CTConfig{}, "", err
	}
	status, err := client.Status(ctx, a.CTID)
	if err != nil {
		return proxmox.CTConfig{}, "", err
	}
	if status.Status != "running" && status.Status != "stopped" {
		return proxmox.CTConfig{}, "", proxmox.ErrUncertain
	}
	return config, status.Status, nil
}

func (m *managedProvider) launch(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	var upid string
	var err error
	switch a.Stage {
	case "create":
		image, clone, imageErr := m.workerImage(ctx, a)
		switch {
		case imageErr != nil:
			err = imageErr
		case clone:
			upid, err = client.Clone(ctx, imageOwned(image), m.agentSpec(a))
		default:
			upid, err = client.Create(ctx, m.agentSpec(a))
		}
	case "start":
		// Стадию start записывает beginStart: Start ещё не запрошен.
		return m.launchStart(ctx, client, a, "")
	case "stop":
		return m.launchStop(ctx, client, a)
	case "delete":
		return m.launchDelete(ctx, client, a)
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
		m.reconcilePass(parent, a)
	}
}

// reconcilePass — один проход reconcile актора a со своим сроком; ошибка
// прохода становится диагностикой актора. Стадия, чью запись PVE ещё не
// получал (notRequested), сохраняет фазу и отметку незапрошенной стадии, у
// остальных фаза — error. Вызывающий держит lifecycle.
func (m *managedProvider) reconcilePass(parent context.Context, a store.ManagedActor) {
	ctx, cancel := context.WithTimeout(parent, m.actorPassTimeout())
	_, err := m.reconcileActor(ctx, a)
	cancel()
	if err == nil || parent.Err() != nil {
		return
	}
	current, getErr := m.server.store.GetManagedActor(parent, a.UID)
	if getErr != nil || current.Revision != a.Revision || current.Phase == "deleted" {
		return
	}
	// Идущая задача — не исход: стадия, UPID и ревизия остаются,
	// следующий проход продолжит ждать тот же UPID.
	if errors.Is(err, errManagedTaskPending) && m.pendingQuiet(current) {
		return
	}
	// Незапрошенные start, stop и delete не стали неизвестными: проход
	// отказал до записи PVE (чтение PVE или запись bootstrap env), CT в
	// прежнем состоянии, следующий проход повторит. Фаза и отметка
	// незапрошенной стадии сохраняются, меняется только диагностика: без
	// отметки стадия считалась бы отправленной навсегда (#277).
	phase, diagnostic := "error", managedProviderError(err)
	if notRequested(current) {
		phase = current.Phase
		diagnostic = store.ManagedUnsentError(current.Unsent(), diagnostic)
	}
	m.saveObservation(parent, current, phase, "", diagnostic)
}

// startNotRequested — стадия start, чей Start ещё не запрошен: CT
// остановлена (phase stopped), задачи нет. Так её записывают beginStart и
// подтверждённая остановка update (#133), так её возвращает отказ до Start.
// Запрос отмечается до отправки: launchStart ставит provisioning и только
// потом зовёт Start. Наблюдение CT эти случаи не различает: Start, чей ответ
// потерян, может ещё не поднять CT, и повтор по виду остановленной CT
// запустил бы её дважды.
func startNotRequested(a store.ManagedActor) bool {
	return a.Operation == "start" && a.Stage == "start" && a.UPID == "" && a.Phase == "stopped" && a.Desired == "running"
}

// stopNotRequested — стадия stop без задачи, чей Stop ещё не запрошен: так
// её записывает BeginManagedActorOperation (фаза stopping или deleting с
// отметкой store.ManagedStopUnsent, #277), так её оставляет отказ Stop до
// записи (фаза наблюдённой работающей CT running, #275) и возврат
// незапрошенного delete к остановке (ReopenManagedActorStop). launchStop
// снимает отметку и ставит фазу остановки перед каждой отправкой: Stop,
// чей ответ потерян или чей UPID не записан, по ней второй раз не уходит.
// Stop-стадия без отметки в фазе остановки — запись до #277 или отправка, и
// исход её неизвестен.
func stopNotRequested(a store.ManagedActor) bool {
	if a.Stage != "stop" || a.UPID != "" {
		return false
	}
	return a.Phase == "running" || ((a.Phase == "stopping" || a.Phase == "deleting") && a.Unsent() == store.ManagedStopUnsent)
}

// deleteNotRequested — стадия delete без задачи, чей Delete ещё не запрошен
// (#277): CT остановлена (phase stopped), отметка store.ManagedDeleteUnsent.
// Так её записывает подтверждённая остановка очистки
// (ConfirmManagedActorStage) и возвращает отказ Delete до записи
// (MarkManagedActorDeleteUnsent). launchDelete снимает отметку перед
// отправкой. Delete-стадия без отметки, в том числе записанная до #277,
// могла отправить Delete и не повторяется: одна фаза stopped этого не
// доказывает.
func deleteNotRequested(a store.ManagedActor) bool {
	return a.Stage == "delete" && a.UPID == "" && a.Phase == "stopped" && a.Unsent() == store.ManagedDeleteUnsent
}

// notRequested — стадия, чью запись PVE ещё не получал: reconcile повторяет
// её каждым проходом, а phase хранит наблюдённое состояние CT.
func notRequested(a store.ManagedActor) bool {
	return startNotRequested(a) || stopNotRequested(a) || deleteNotRequested(a)
}

// unsent сообщает, что запись PVE отказала, ничего не отправив (proxmoxAPI).
func unsent(upid string, err error) bool {
	return err != nil && upid == "" && !errors.Is(err, proxmox.ErrUncertain)
}

// waitingRefusal refuses a lifecycle change while a start, stop or delete
// stage is not yet requested from PVE (notRequested). It names the wait and
// the last failure instead of an unknown outcome: no task is in flight. stop,
// delete or reset cancel a waiting start, whose container is stopped; a
// waiting stop or delete is the change that has to happen first. phase is
// the stage's phase: running or stopped is the container a pass observed;
// a stop that only its intent recorded (stopping or deleting with
// store.ManagedStopUnsent) has no observation, and its container may have
// stopped or vanished since. last is the public diagnostic without the
// unsent mark (store.ManagedActor.Diagnostic).
type waitingRefusal struct {
	action, name, stage, phase, last string
}

func (e *waitingRefusal) Error() string {
	verb, request, cancel := "start", "Start", "; stop, delete or reset cancels it"
	switch e.stage {
	case "stop":
		verb, request, cancel = "stop", "Stop", ""
	case "delete":
		verb, request, cancel = "delete", "Delete", ""
	}
	container := "its container"
	if e.phase == "running" || e.phase == "stopped" {
		container = "its " + e.phase + " container"
	}
	last := request + " is not yet requested"
	if e.last != "" {
		last = "last attempt failed: " + e.last
	}
	return fmt.Sprintf("%s refused: actor %q is waiting to %s %s (%s); reconcile retries the %s on every pass%s", e.action, e.name, verb, container, last, verb, cancel)
}

func (e *waitingRefusal) Unwrap() error { return app.ErrConflict }

func newWaitingRefusal(action string, a store.ManagedActor) error {
	return &waitingRefusal{action: action, name: a.Name, stage: a.Stage, phase: a.Phase, last: a.Diagnostic()}
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
	// Незапрошенный Start не отправлялся: его можно запросить.
	if startNotRequested(a) {
		return m.resumeStart(ctx, client, a)
	}
	// Незапрошенный Delete тоже, после наблюдения CT (#277).
	if deleteNotRequested(a) {
		observed, err := managedObservedState(ctx, client, a)
		if err != nil {
			return a, err
		}
		switch observed {
		case "absent":
			return m.finishConfirmedCleanup(ctx, client, a)
		case "running":
			// CT запустили мимо brood: Delete работающей CT не уходит, а
			// исход не неизвестен — Delete не запрашивался. Стадия
			// возвращается к незапрошенной остановке той же операции, и
			// владение уже проверено наблюдением.
			a, err = m.server.store.ReopenManagedActorStop(ctx, a.UID, a.Revision, a.OperationID)
			if err != nil {
				return a, err
			}
			return m.launchStop(ctx, client, a)
		}
		return m.launchDelete(ctx, client, a)
	}
	// Незапрошенный Stop тоже: наблюдение ниже решает, нужен ли он.
	waitingStop := stopNotRequested(a)
	// A missing task response is not proof that no task is still running. Never
	// replay a submitted stage, even if the CT is currently stopped or absent.
	if a.Stage != "done" && a.UPID == "" && !waitingStop {
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
		if _, clone, _ := m.workerImage(ctx, a); clone {
			// Клон наследует ресурсы сборочного контейнера: до первого
			// старта ему задаётся доля воркера. Повтор безопасен.
			if err := client.Resize(ctx, managedOwned(a), int(a.Resources.CPUMillicores/1000), int(a.Resources.MemoryBytes/mib)); err != nil {
				return a, err
			}
		}
		a, err = m.beginStart(ctx, a, store.NewID())
		if err != nil {
			return a, err
		}
		return m.launch(ctx, client, a)
	case "stop", "delete":
		if observed == "running" {
			if waitingStop {
				return m.launchStop(ctx, client, a)
			}
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
		if observed == "absent" && (a.Operation == "delete" || a.Operation == "reset") {
			return m.cleanupAbsent(ctx, client, a)
		}
		a, err = m.server.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
		if err != nil || a.Stage == "done" {
			return a, err
		}
		// Start update идёт следующим проходом со своим сроком: этот срок
		// мог уйти на ожидание stop-задачи.
		if startNotRequested(a) {
			return a, nil
		}
		return m.launch(ctx, client, a)
	case "start":
		if observed != "running" {
			return a, proxmox.ErrUncertain
		}
		if a.Stage != "done" {
			a, err = m.confirmStartRunning(ctx, a)
			if err != nil {
				return a, err
			}
		}
		if m.hub == nil || !m.hub.Connected(a.UID, a.Incarnation) || !m.probe(ctx, a) {
			// Агента в контейнер ставит сервер по SSH; пока контейнер не
			// отвечает, это ожидание, отказ установки — диагностика воркера.
			if err := m.installAgent(ctx, client, a); err != nil && !errors.Is(err, errAgentWaiting) {
				return m.saveObservation(ctx, a, "error", "", err.Error())
			}
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

// confirmStartRunning закрывает стадию start наблюдённой работающей CT.
// Работающая CT не зависит от readiness гостя: task fence фиксируется без
// публикации running до настоящего ttyd probe (reconcileActor).
func (m *managedProvider) confirmStartRunning(ctx context.Context, a store.ManagedActor) (store.ManagedActor, error) {
	err := m.server.store.WithTx(ctx, func(tx context.Context) error {
		var e error
		a, e = m.server.store.ConfirmManagedActorStage(tx, a.UID, a.Revision, a.OperationID, a.Stage, "running")
		if e == nil {
			a, e = m.saveObservation(tx, a, "provisioning", "", "")
		}
		return e
	})
	return a, err
}

// beginStart записывает намерение start остановленной CT незапрошенным
// (startNotRequested, #275): BeginManagedActorOperation ставит provisioning,
// а фаза stopped в той же транзакции говорит, что Start не отправлялся. Сбой
// до Start, в том числе остановка сервера, оставляет start следующему
// проходу, как start update (#133); provisioning ставит launchStart перед
// самим Start.
func (m *managedProvider) beginStart(ctx context.Context, a store.ManagedActor, operationID string) (store.ManagedActor, error) {
	err := m.server.store.WithTx(ctx, func(tx context.Context) error {
		var e error
		a, e = m.server.store.BeginManagedActorOperation(tx, a.UID, a.Revision, operationID, "start")
		if e == nil {
			a, e = m.saveObservation(tx, a, "stopped", "", "")
		}
		return e
	})
	return a, err
}

// resumeStart запрашивает start, чей Start ещё не запрошен
// (startNotRequested). Остановленная CT получает start; исчезнувшая CT кончает
// операцию ошибкой, как после stop-стадии. Работающую CT запустили мимо
// brood (#277): Start этой операции не отправлялся, и задачи PVE, чей исход
// неизвестен, нет. Стадия согласуется наблюдением без записи в PVE, как
// start работающей CT (confirmStartRunning), а running публикует только
// проверка готовности гостя следующим проходом.
func (m *managedProvider) resumeStart(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	config, observed, err := managedObservation(ctx, client, a)
	if err != nil {
		return a, err
	}
	switch observed {
	case "stopped":
		return m.launchStart(ctx, client, a, config.Digest)
	case "absent":
		return m.server.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
	case "running":
		return m.confirmStartRunning(ctx, a)
	}
	return a, proxmox.ErrUncertain
}

// launchStart отправляет незапрошенный start (startNotRequested) CT,
// наблюдённой остановленной с digest конфигурации digest ("" — digest не
// известен). Отказ до Start (чтения владения, digest и статуса, отказ или
// неоднозначный исход записи bootstrap) оставляет start незапрошенным с
// диагностикой для следующего прохода. Bootstrap, уже записанный этой
// операцией, не переписывается, пока конфигурация не менялась
// (bootstrapCurrent): Start, отказывающий явно, повторяется каждым проходом,
// а запись конфигурации — нет. Перед Start отметка provisioning фиксирует
// запрос: Start, отправленный до остановки сервера, по ней не уходит второй
// раз.
func (m *managedProvider) launchStart(ctx context.Context, client proxmoxAPI, a store.ManagedActor, digest string) (store.ManagedActor, error) {
	owned := managedOwned(a)
	a, err := m.saveObservation(ctx, a, "provisioning", "", "")
	if err != nil {
		return a, err
	}
	upid, err := client.Start(ctx, owned)
	if unsent(upid, err) {
		// Start отказал на чтении владения или явным 403/409: задачи нет, и
		// start снова ждёт запроса. Возврат к stopped переживает отмену
		// прохода.
		durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return m.leavePending(durable, a, "stopped", err)
	}
	return m.recordTask(ctx, a, upid, err)
}

// launchStop отправляет Stop стадии stop CT, наблюдённой работающей.
// Незапрошенный Stop (stopNotRequested) сначала снова получает фазу
// остановки, как от BeginManagedActorOperation: Stop, чей ответ потерян или
// чей UPID не записан, по ней второй раз не уходит. Отказ Stop до записи
// (чтение владения, явный 403/409) возвращает стадии фазу running: задачи
// нет, и следующий проход повторит Stop (#275).
func (m *managedProvider) launchStop(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	if stopNotRequested(a) {
		marked, err := m.saveObservation(ctx, a, stopPhase(a), "", "")
		if err != nil {
			return a, err
		}
		a = marked
	}
	upid, err := client.Stop(ctx, managedOwned(a))
	if unsent(upid, err) {
		durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return m.leavePending(durable, a, "running", err)
	}
	return m.recordTask(ctx, a, upid, err)
}

// launchDelete отправляет незапрошенный Delete (deleteNotRequested) CT,
// наблюдённой остановленной. Перед отправкой отметка незапрошенной стадии
// снимается, а фаза становится фазой запрошенного Delete (deletePhase):
// Delete, чей ответ потерян или чей UPID не записан, по ней второй раз не
// уходит. Только отказ до записи (чтение владения, явный 403/409)
// возвращает отметку (MarkManagedActorDeleteUnsent), и следующий проход
// повторит Delete. Delete-стадия без отметки не отправляется: её исход
// неизвестен.
func (m *managedProvider) launchDelete(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	if !deleteNotRequested(a) {
		return a, proxmox.ErrUncertain
	}
	marked, err := m.saveObservation(ctx, a, deletePhase(a), "", "")
	if err != nil {
		return a, err
	}
	a = marked
	upid, err := client.Delete(ctx, managedOwned(a))
	if unsent(upid, err) {
		durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		saved, saveErr := m.server.store.MarkManagedActorDeleteUnsent(durable, a.UID, a.Revision, a.OperationID, managedProviderError(err))
		if saveErr != nil {
			return a, err
		}
		return saved, nil
	}
	return m.recordTask(ctx, a, upid, err)
}

// stopPhase — фаза запрошенной стадии stop, как её ставит
// BeginManagedActorOperation: deleting у delete, stopping у остальных.
func stopPhase(a store.ManagedActor) string {
	if a.Operation == "delete" {
		return "deleting"
	}
	return "stopping"
}

// deletePhase — фаза запрошенной стадии delete: deleting у delete и stopped
// у reset, как до #277: CT остановлена, а старая инкарнация reset ещё
// получает ответы на свои вопросы (actorAnswerable), пока её CT удаляют.
func deletePhase(a store.ManagedActor) string {
	if a.Operation == "delete" {
		return "deleting"
	}
	return "stopped"
}

// leavePending оставляет стадию незапрошенной (notRequested) с фазой phase и
// диагностикой отказа err: отказ до записи PVE — состояние актора, а не
// ошибка прохода. Если записать не удалось, возвращается err: диагностику
// сохранит проход (reconcilePass), а неснятая отметка отправки оставит исход
// неизвестным.
func (m *managedProvider) leavePending(ctx context.Context, a store.ManagedActor, phase string, err error) (store.ManagedActor, error) {
	saved, saveErr := m.saveObservation(ctx, a, phase, "", managedProviderError(err))
	if saveErr != nil {
		return a, err
	}
	return saved, nil
}

// managedBootstrap — подтверждённая запись bootstrap env стадии start:
// операция operationID записала env (его SHA-256: значения секретны), и
// после записи конфигурация CT имела digest. Запись живёт в памяти процесса:
// после перезапуска сервера первый повтор пишет env снова.
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
	a, err := m.releaseCleanup(ctx, a)
	if err != nil || a.Operation != "reset" {
		return a, err
	}
	return m.launch(ctx, client, a)
}

// releaseCleanup — запись в базе после подтверждённого отсутствия CT стадии
// delete: delete освобождает аллокацию, reset начинает новую инкарнацию.
// PVE не зовётся: Create reset отправляет вызывающий после фиксации.
func (m *managedProvider) releaseCleanup(ctx context.Context, a store.ManagedActor) (store.ManagedActor, error) {
	switch a.Operation {
	case "delete":
		return m.server.store.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID)
	case "reset":
		return m.server.store.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, store.NewID())
	}
	return a, app.ErrConflict
}

// cleanupAbsent кончает стадию stop очистки (delete, reset), чья CT
// подтверждённо отсутствует: стадия закрывается и аллокация освобождается
// (у reset — начинается новая инкарнация) одной транзакцией, затем reset
// отправляет Create. Отдельная фиксация закрытой стадии оставила бы при
// остановке сервера стадию delete без задачи и без отметки — её исход
// неизвестен навсегда, хотя Delete не отправлялся (#277). При отказе стадия
// stop остаётся, и следующий проход снова находит CT отсутствующей.
func (m *managedProvider) cleanupAbsent(ctx context.Context, client proxmoxAPI, a store.ManagedActor) (store.ManagedActor, error) {
	var released store.ManagedActor
	err := m.server.store.WithTx(ctx, func(tx context.Context) error {
		confirmed, e := m.server.store.ConfirmManagedActorStage(tx, a.UID, a.Revision, a.OperationID, a.Stage, "absent")
		if e == nil {
			released, e = m.releaseCleanup(tx, confirmed)
		}
		return e
	})
	if err != nil {
		return a, err
	}
	if released.Operation != "reset" {
		return released, nil
	}
	return m.launch(ctx, client, released)
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
func managedActorJSON(a store.ManagedActor, p store.ManagedPool, swarmID, originMode string) map[string]any {
	state := "ACTOR_STATE_UNSPECIFIED"
	// Stop, ещё не запрошенный у PVE (#275), — та же остановка, что stopping
	// или deleting: CT работает лишь до запроса. Незапрошенный Delete (#277)
	// — то же удаление, что запрошенный. phase остаётся наблюдением CT, state
	// называет идущее изменение.
	lifecycle := a.Phase
	switch {
	case stopNotRequested(a):
		lifecycle = stopPhase(a)
	case deleteNotRequested(a):
		lifecycle = deletePhase(a)
	}
	switch lifecycle {
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
	return map[string]any{"metadata": map[string]any{"name": a.Name, "uid": a.UID, "version": managedDecimal(a.Revision), "swarm": swarmID}, "actorTemplate": map[string]any{"name": managedTemplatePrefix + p.Name}, "provider": "proxmox", "role": a.Role, "profile": a.Template.Profile, "generation": managedDecimal(a.Incarnation), "phase": a.Phase, "desired": a.Desired, "placement": map[string]any{"node": p.Source.Node, "ctid": a.CTID, "pool": p.Name}, "capabilities": managedCapabilities(), "templateDefinition": map[string]any{"profile": a.Template.Profile, "digest": a.Template.Digest, "resources": map[string]string{"cpuMillicores": managedDecimal(a.Resources.CPUMillicores), "memoryBytes": managedDecimal(a.Resources.MemoryBytes), "diskBytes": managedDecimal(a.Resources.DiskBytes)}}, "status": map[string]any{"state": state, "errorMessage": a.Diagnostic()}, "origin": a.Origin, "originMode": originMode}
}

func (m *managedProvider) actorsJSON(ctx context.Context, sw app.Swarm) ([]any, error) {
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
		p, err := m.server.store.GetManagedPool(ctx, a.PoolUID)
		if err != nil {
			return nil, err
		}
		out = append(out, managedActorJSON(a, p, sw.ID, access.mode(a.Origin)))
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
