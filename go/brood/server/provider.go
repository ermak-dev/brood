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
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// proxmoxAPI is the PVE client of the provider (proxmox.Client). A
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

type providerRuntime struct {
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
	// Швы прохода reconcile: nil и 0 значат time.Now и defaultPassTimeout.
	now         func() time.Time
	passTimeout time.Duration
}

// defaultPassTimeout — срок одного прохода reconcile для одного актора.
// Задача PVE может идти дольше: OCI vzcreate на живом стенде шёл 34 с.
const defaultPassTimeout = 20 * time.Second

// taskPendingLimit — сколько идущая задача ждёт без диагностики.
// Отсчёт от UpdatedAt актора: привязка UPID его обновляет.
const taskPendingLimit = 15 * time.Minute

const taskPendingError = "Proxmox is still working on the operation. brood keeps waiting; no action is needed."

// errTaskPending — задача ещё идёт; истёк только срок прохода.
var errTaskPending = errors.New("Proxmox task is still running")

func (m *providerRuntime) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

func (m *providerRuntime) actorPassTimeout() time.Duration {
	if m.passTimeout <= 0 {
		return defaultPassTimeout
	}
	return m.passTimeout
}

// taskPending отличает идущую задачу от исхода: WaitTask прервал только
// истёкший срок этого прохода. Таймаут HTTP-клиента, отмена, сбой сети и отказ
// PVE не делают задачу «ещё идущей».
func taskPending(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// pendingQuiet сообщает, что идущую задачу пока не показывают ошибкой: предел
// ожидания не истёк или диагностика уже записана. Нечитаемая отметка времени
// не продлевает ожидание.
func (m *providerRuntime) pendingQuiet(a store.Agent) bool {
	if a.Phase == "error" && a.Error == taskPendingError {
		return true
	}
	since, err := time.Parse(time.RFC3339Nano, a.UpdatedAt)
	return err == nil && m.clock().Sub(since) < taskPendingLimit
}

func (m *provider) initialize(cfg Config) error {
	rows, err := m.server.store.ListConnections(context.Background())
	if err != nil {
		return errors.New("Proxmox connections unavailable")
	}
	for _, c := range rows {
		if m.registry != nil {
			if _, exists := m.registry.pools[c.Name]; exists {
				return errors.New("Proxmox connection name conflicts with environment configuration")
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
func (m *provider) start(ctx context.Context) {
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
func (m *provider) stopReconcile() {
	if m.cancel != nil {
		m.cancel()
		m.workers.Wait()
	}
}
func (m *provider) close() {
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
	mac.Write([]byte("brood/proxmox/guest/v1\x00" + uid + "\x00" + decimal(generation)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (m *provider) authorizeGuest(uid string, generation int64, token string) bool {
	if m.registry == nil || len(m.registry.relaySecret) < 32 || generation <= 0 {
		return false
	}
	expected := guestToken(m.registry.relaySecret, uid, generation)
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a, err := m.server.store.GetAgent(ctx, uid)
	// Only a start boots the guest. Create leaves the CT stopped, and a reset
	// keeps the old incarnation until cleanup, even after a failed stage settles.
	return err == nil && a.Phase != "deleted" && a.Desired == "running" && a.Operation == "start" && a.Incarnation == generation
}

func ownedCT(a store.Agent) proxmox.OwnedCT {
	return proxmox.OwnedCT{VMID: a.CTID, Owner: proxmox.Ownership{PoolUID: a.PoolUID, ActorUID: a.UID, Incarnation: uint64(a.Incarnation), Nonce: a.OwnerNonce}}
}

func (m *provider) client(p store.Pool) (proxmoxAPI, error) {
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
		secret, err = m.server.store.ConnectionSecret(context.Background(), p.Connection)
		if err != nil {
			return nil, fmt.Errorf("%w: Proxmox credential unavailable", app.ErrConflict)
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

func (m *provider) saveObservation(ctx context.Context, a store.Agent, phase, upid, detail string) (store.Agent, error) {
	a.Phase = phase
	a.Error = detail
	if upid != "" {
		a.UPID = upid
	}
	return m.server.store.UpdateAgent(ctx, a, a.Revision)
}
func (m *provider) recordTask(ctx context.Context, a store.Agent, upid string, err error) (store.Agent, error) {
	// Provider evidence must survive the browser disconnecting after submission.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if upid != "" {
		var bindErr error
		a, bindErr = m.server.store.BindAgentTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, upid)
		if bindErr != nil {
			return a, bindErr
		}
	}
	if err != nil {
		return m.saveObservation(ctx, a, "error", "", providerError(err))
	}
	return a, nil
}

// Диагностика агента для человека: что произошло, изменилось ли что-нибудь
// и что делать дальше. Код рядом с текстом позволяет панели показать
// перевод и подсказку; сырой ответ провайдера сюда не попадает.
const (
	ownershipError    = "The container in Proxmox no longer matches what brood created: its description, disk or network was changed, or it left the brood resource pool. brood changed nothing. Restore the container in Proxmox or delete the agent."
	permissionError   = "Proxmox refused the operation because the brood API token lacks a permission. Nothing was changed. An administrator should check the roles of the token, then retry."
	taskFailedError   = "Proxmox started the operation and reported that it failed. Open the task log of the container in Proxmox for the reason, then retry or delete the agent."
	agentWaitingError = "The container is running but does not answer over SSH yet. brood installs the agent as soon as it answers; no action is needed."
	uncertainError    = "Proxmox did not confirm whether the operation started: the request failed or timed out. brood does not repeat it blindly. If the container does not exist in Proxmox, delete the agent; otherwise wait and reload."
	unavailableError  = "brood could not reach Proxmox to check the container. It retries automatically; no action is needed unless this persists."
)

func providerError(err error) string {
	switch {
	case errors.Is(err, proxmox.ErrOwnership):
		return ownershipError
	case errors.Is(err, proxmox.ErrPermission):
		return permissionError
	case errors.Is(err, proxmox.ErrTaskFailed):
		return taskFailedError
	case errors.Is(err, errTaskPending):
		return taskPendingError
	case errors.Is(err, errAgentWaiting):
		return agentWaitingError
	case errors.Is(err, proxmox.ErrUncertain):
		return uncertainError
	default:
		return unavailableError
	}
}

// diagnosticCode — устойчивый код диагностики для панели; пусто — текст
// без известного кода.
func diagnosticCode(message string) string {
	switch message {
	case "":
		return ""
	case ownershipError:
		return "ownership"
	case permissionError:
		return "permission"
	case taskFailedError:
		return "task_failed"
	case taskPendingError:
		return "task_pending"
	case agentWaitingError:
		return "agent_waiting"
	case uncertainError:
		return "uncertain"
	case unavailableError:
		return "provider_unavailable"
	case readinessError:
		return "agent_not_connected"
	}
	if strings.HasPrefix(message, "agent installation failed") {
		return "agent_install"
	}
	return "other"
}

func inspectAgent(ctx context.Context, client proxmoxAPI, a store.Agent) (proxmox.CTConfig, error) {
	return client.InspectOwned(ctx, ownedCT(a))
}

func observedState(ctx context.Context, client proxmoxAPI, a store.Agent) (string, error) {
	_, observed, err := observation(ctx, client, a)
	return observed, err
}

// observation — observedState вместе с проверенной владением
// конфигурацией CT; у отсутствующей CT конфигурация пуста.
func observation(ctx context.Context, client proxmoxAPI, a store.Agent) (proxmox.CTConfig, string, error) {
	config, err := inspectAgent(ctx, client, a)
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

func (m *provider) launch(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
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
			if err == nil {
				// PVE держит замок на шаблоне, пока идёт клон: второй клон
				// того же образа в это время получил бы отказ с неизвестным
				// исходом. Вызывающий держит lifecycle, поэтому ожидание
				// задачи здесь выстраивает клоны в очередь. Исход задачи
				// читает reconcile по записанному UPID.
				_ = client.WaitTask(ctx, upid)
			}
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
		return a, fmt.Errorf("%w: unsupported agent intent", app.ErrConflict)
	}
	// An API error is durable state, not permission to retry a write or pick a new CTID.
	return m.recordTask(ctx, a, upid, err)
}

func (m *provider) reconcileOnce(parent context.Context) {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	actors, err := m.server.store.ListAgents(parent, "")
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
func (m *provider) reconcilePass(parent context.Context, a store.Agent) {
	ctx, cancel := context.WithTimeout(parent, m.actorPassTimeout())
	_, err := m.reconcileActor(ctx, a)
	cancel()
	if err == nil || parent.Err() != nil {
		return
	}
	current, getErr := m.server.store.GetAgent(parent, a.UID)
	if getErr != nil || current.Revision != a.Revision || current.Phase == "deleted" {
		return
	}
	// Идущая задача — не исход: стадия, UPID и ревизия остаются,
	// следующий проход продолжит ждать тот же UPID.
	if errors.Is(err, errTaskPending) && m.pendingQuiet(current) {
		return
	}
	// Незапрошенные start, stop и delete не стали неизвестными: проход
	// отказал до записи PVE (чтение PVE или запись bootstrap env), CT в
	// прежнем состоянии, следующий проход повторит. Фаза и отметка
	// незапрошенной стадии сохраняются, меняется только диагностика: без
	// отметки стадия считалась бы отправленной навсегда (#277).
	phase, diagnostic := "error", providerError(err)
	if notRequested(current) {
		phase = current.Phase
		diagnostic = store.JoinUnsent(current.Unsent(), diagnostic)
	}
	m.saveObservation(parent, current, phase, "", diagnostic)
}

// startNotRequested — стадия start, чей Start ещё не запрошен: CT
// остановлена (phase stopped), задачи нет. Так её записывает beginStart,
// так её возвращает отказ до Start.
// Запрос отмечается до отправки: launchStart ставит provisioning и только
// потом зовёт Start. Наблюдение CT эти случаи не различает: Start, чей ответ
// потерян, может ещё не поднять CT, и повтор по виду остановленной CT
// запустил бы её дважды.
func startNotRequested(a store.Agent) bool {
	return a.Operation == "start" && a.Stage == "start" && a.UPID == "" && a.Phase == "stopped" && a.Desired == "running"
}

// stopNotRequested — стадия stop без задачи, чей Stop ещё не запрошен: так
// её записывает BeginAgentOperation (фаза stopping или deleting с
// отметкой store.StopUnsent, #277), так её оставляет отказ Stop до
// записи (фаза наблюдённой работающей CT running, #275) и возврат
// незапрошенного delete к остановке (ReopenAgentStop). launchStop
// снимает отметку и ставит фазу остановки перед каждой отправкой: Stop,
// чей ответ потерян или чей UPID не записан, по ней второй раз не уходит.
// Stop-стадия без отметки в фазе остановки — запись до #277 или отправка, и
// исход её неизвестен.
func stopNotRequested(a store.Agent) bool {
	if a.Stage != "stop" || a.UPID != "" {
		return false
	}
	return a.Phase == "running" || ((a.Phase == "stopping" || a.Phase == "deleting") && a.Unsent() == store.StopUnsent)
}

// deleteNotRequested — стадия delete без задачи, чей Delete ещё не запрошен
// (#277): CT остановлена (phase stopped), отметка store.DeleteUnsent.
// Так её записывает подтверждённая остановка очистки
// (ConfirmAgentStage) и возвращает отказ Delete до записи
// (MarkAgentDeleteUnsent). launchDelete снимает отметку перед
// отправкой. Delete-стадия без отметки, в том числе записанная до #277,
// могла отправить Delete и не повторяется: одна фаза stopped этого не
// доказывает.
func deleteNotRequested(a store.Agent) bool {
	return a.Stage == "delete" && a.UPID == "" && a.Phase == "stopped" && a.Unsent() == store.DeleteUnsent
}

// notRequested — стадия, чью запись PVE ещё не получал: reconcile повторяет
// её каждым проходом, а phase хранит наблюдённое состояние CT.
func notRequested(a store.Agent) bool {
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
// store.StopUnsent) has no observation, and its container may have
// stopped or vanished since. last is the public diagnostic without the
// unsent mark (store.Agent.Diagnostic).
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
	return fmt.Sprintf("Agent %q is already waiting to %s %s (%s). brood retries the %s automatically%s.", e.name, verb, container, last, verb, cancel)
}

func (e *waitingRefusal) Unwrap() error { return app.ErrConflict }

func newWaitingRefusal(action string, a store.Agent) error {
	return &waitingRefusal{action: action, name: a.Name, stage: a.Stage, phase: a.Phase, last: a.Diagnostic()}
}

func (m *provider) reconcileActor(ctx context.Context, a store.Agent) (result store.Agent, err error) {
	defer func() {
		if !errors.Is(err, store.ErrRevisionConflict) && (err != nil || result.Phase != "running") {
			m.verified.Delete(a.UID)
		}
	}()
	current, err := m.server.store.GetAgent(ctx, a.UID)
	if err != nil {
		return a, err
	}
	if current.Revision != a.Revision {
		return a, store.ErrRevisionConflict
	}
	// Согласованный терминальный отказ требует нового явного намерения.
	// Диагностика сохраняется даже при недоступной конфигурации провайдера.
	if a.Stage == "done" && a.Phase == "error" && a.Error == providerError(proxmox.ErrTaskFailed) {
		return a, nil
	}
	p, err := m.server.store.GetPool(ctx, a.PoolUID)
	if err != nil {
		return a, err
	}
	client, err := m.client(p)
	if err != nil {
		return a, err
	}
	// Воркер из базового шаблона обязан совпадать с шаблоном пула; клон
	// образа проекта называет свой образ, и владение им проверяет клиент.
	if _, clone := store.ProjectImageFromURI(a.Template.URI); !clone && a.Template != p.Template {
		return a, proxmox.ErrOwnership
	}
	// Незапрошенный Start не отправлялся: его можно запросить.
	if startNotRequested(a) {
		return m.resumeStart(ctx, client, a)
	}
	// Незапрошенный Delete тоже, после наблюдения CT (#277).
	if deleteNotRequested(a) {
		observed, err := observedState(ctx, client, a)
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
			a, err = m.server.store.ReopenAgentStop(ctx, a.UID, a.Revision, a.OperationID)
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
	// Done фиксирует согласование task, включая отказ. Сохранённый UPID —
	// доказательство, а не повод читать заново уже истёкшие логи PVE.
	if a.Stage != "done" && a.UPID != "" {
		if err := client.WaitTask(ctx, a.UPID); err != nil {
			if errors.Is(err, proxmox.ErrTaskFailed) {
				return m.resolveTaskFailure(ctx, client, a)
			}
			// Срок прохода короче долгих задач PVE: это ожидание, а не отказ.
			// Ни повторной записи, ни замены UPID.
			if taskPending(ctx, err) {
				return a, errTaskPending
			}
			return a, err
		}
	}
	if a.Stage == "delete" {
		return m.finishCleanup(ctx, client, a)
	}
	observed, err := observedState(ctx, client, a)
	if err != nil {
		return a, err
	}
	stage := a.Stage
	if stage == "done" {
		stage = a.Operation
		if a.Operation == "reset" {
			stage = "create"
		}
	}
	switch stage {
	case "create":
		if observed != "stopped" {
			return a, proxmox.ErrUncertain
		}
		if a.Stage != "done" {
			a, err = m.server.store.ConfirmAgentStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
		} else if a.Phase != "stopped" || a.Error != "" {
			a, err = m.saveObservation(ctx, a, "stopped", "", "")
		}
		if err != nil {
			return a, err
		}
		if _, clone, _ := m.workerImage(ctx, a); clone {
			// Клон наследует ресурсы сборочного контейнера: до первого
			// старта ему задаётся доля воркера. Повтор безопасен.
			if err := client.Resize(ctx, ownedCT(a), int(a.Resources.CPUMillicores/1000), int(a.Resources.MemoryBytes/mib)); err != nil {
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
		a, err = m.server.store.ConfirmAgentStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
		if err != nil || a.Stage == "done" {
			return a, err
		}
		// Незапрошенный start идёт следующим проходом со своим сроком: этот
		// срок мог уйти на ожидание stop-задачи.
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
	return a, fmt.Errorf("%w: unsupported agent reconciliation intent", app.ErrConflict)
}

// resolveTaskFailure is called only after WaitTask returned ErrTaskFailed for
// this exact bound slot. Observation alone never proves task terminality.
func (m *provider) resolveTaskFailure(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
	observed, err := observedState(ctx, client, a)
	if err != nil {
		return a, err
	}
	err = m.server.store.WithTx(ctx, func(tx context.Context) error {
		var e error
		a, e = m.saveObservation(tx, a, "error", "", providerError(proxmox.ErrTaskFailed))
		if e == nil {
			a, e = m.server.store.ResolveAgentTaskFailure(tx, a.UID, a.Revision, a.OperationID, a.Stage, a.UPID, observed)
		}
		return e
	})
	return a, err
}

// confirmStartRunning закрывает стадию start наблюдённой работающей CT.
// Работающая CT не зависит от readiness гостя: task fence фиксируется без
// публикации running до настоящего ttyd probe (reconcileActor).
func (m *provider) confirmStartRunning(ctx context.Context, a store.Agent) (store.Agent, error) {
	err := m.server.store.WithTx(ctx, func(tx context.Context) error {
		var e error
		a, e = m.server.store.ConfirmAgentStage(tx, a.UID, a.Revision, a.OperationID, a.Stage, "running")
		if e == nil {
			a, e = m.saveObservation(tx, a, "provisioning", "", "")
		}
		return e
	})
	return a, err
}

// beginStart записывает намерение start остановленной CT незапрошенным
// (startNotRequested, #275): BeginAgentOperation ставит provisioning,
// а фаза stopped в той же транзакции говорит, что Start не отправлялся. Сбой
// до Start, в том числе остановка сервера, оставляет start следующему
// проходу; provisioning ставит launchStart перед самим Start.
func (m *provider) beginStart(ctx context.Context, a store.Agent, operationID string) (store.Agent, error) {
	err := m.server.store.WithTx(ctx, func(tx context.Context) error {
		var e error
		a, e = m.server.store.BeginAgentOperation(tx, a.UID, a.Revision, operationID, "start")
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
func (m *provider) resumeStart(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
	config, observed, err := observation(ctx, client, a)
	if err != nil {
		return a, err
	}
	switch observed {
	case "stopped":
		return m.launchStart(ctx, client, a, config.Digest)
	case "absent":
		return m.server.store.ConfirmAgentStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
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
func (m *provider) launchStart(ctx context.Context, client proxmoxAPI, a store.Agent, digest string) (store.Agent, error) {
	owned := ownedCT(a)
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
// остановки, как от BeginAgentOperation: Stop, чей ответ потерян или
// чей UPID не записан, по ней второй раз не уходит. Отказ Stop до записи
// (чтение владения, явный 403/409) возвращает стадии фазу running: задачи
// нет, и следующий проход повторит Stop (#275).
func (m *provider) launchStop(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
	if stopNotRequested(a) {
		marked, err := m.saveObservation(ctx, a, stopPhase(a), "", "")
		if err != nil {
			return a, err
		}
		a = marked
	}
	upid, err := client.Stop(ctx, ownedCT(a))
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
// возвращает отметку (MarkAgentDeleteUnsent), и следующий проход
// повторит Delete. Delete-стадия без отметки не отправляется: её исход
// неизвестен.
func (m *provider) launchDelete(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
	if !deleteNotRequested(a) {
		return a, proxmox.ErrUncertain
	}
	marked, err := m.saveObservation(ctx, a, deletePhase(a), "", "")
	if err != nil {
		return a, err
	}
	a = marked
	upid, err := client.Delete(ctx, ownedCT(a))
	if unsent(upid, err) {
		durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		saved, saveErr := m.server.store.MarkAgentDeleteUnsent(durable, a.UID, a.Revision, a.OperationID, providerError(err))
		if saveErr != nil {
			return a, err
		}
		return saved, nil
	}
	return m.recordTask(ctx, a, upid, err)
}

// stopPhase — фаза запрошенной стадии stop, как её ставит
// BeginAgentOperation: deleting у delete, stopping у остальных.
func stopPhase(a store.Agent) string {
	if a.Operation == "delete" {
		return "deleting"
	}
	return "stopping"
}

// deletePhase — фаза запрошенной стадии delete: deleting у delete и stopped
// у reset, как до #277: CT остановлена, а старая инкарнация reset ещё
// получает ответы на свои вопросы (actorAnswerable), пока её CT удаляют.
func deletePhase(a store.Agent) string {
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
func (m *provider) leavePending(ctx context.Context, a store.Agent, phase string, err error) (store.Agent, error) {
	saved, saveErr := m.saveObservation(ctx, a, phase, "", providerError(err))
	if saveErr != nil {
		return a, err
	}
	return saved, nil
}

// readinessWait — с какого момента операция start ждёт подключения агента.
// Запись живёт в памяти процесса: после перезапуска сервера отсчёт
// начинается снова.
type readinessWait struct {
	operationID string
	since       time.Time
}

const readinessError = "The agent did not connect within two minutes after its container started. Stop and start the agent; if it repeats, check the brood-agent service in the container console in Proxmox."

func (m *provider) waitReadiness(ctx context.Context, a store.Agent) (store.Agent, error) {
	value, _ := m.readiness.LoadOrStore(a.UID, readinessWait{a.OperationID, time.Now()})
	waiting := value.(readinessWait)
	if waiting.operationID != a.OperationID {
		waiting = readinessWait{a.OperationID, time.Now()}
		m.readiness.Store(a.UID, waiting)
	}
	phase, detail := "provisioning", ""
	if time.Since(waiting.since) >= 2*time.Minute {
		phase, detail = "error", readinessError
	}
	if a.Phase == phase && a.Error == detail {
		return a, nil
	}
	return m.saveObservation(ctx, a, phase, "", detail)
}

func (m *provider) finishCleanup(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
	// Config 404 and ACL-filtered inventory are not global absence evidence.
	if err := client.ConfirmAbsent(ctx, a.CTID); err != nil {
		return a, err
	}
	return m.finishConfirmedCleanup(ctx, client, a)
}

// finishConfirmedCleanup consumes an already-positive global absence proof.
func (m *provider) finishConfirmedCleanup(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
	a, err := m.releaseCleanup(ctx, a)
	if err != nil || a.Operation != "reset" {
		return a, err
	}
	return m.launch(ctx, client, a)
}

// releaseCleanup — запись в базе после подтверждённого отсутствия CT стадии
// delete: delete освобождает аллокацию, reset начинает новую инкарнацию.
// PVE не зовётся: Create reset отправляет вызывающий после фиксации.
func (m *provider) releaseCleanup(ctx context.Context, a store.Agent) (store.Agent, error) {
	switch a.Operation {
	case "delete":
		return m.server.store.ConfirmAgentDeleted(ctx, a.UID, a.Revision, a.OperationID)
	case "reset":
		return m.server.store.ConfirmAgentReset(ctx, a.UID, a.Revision, a.OperationID, store.NewID())
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
func (m *provider) cleanupAbsent(ctx context.Context, client proxmoxAPI, a store.Agent) (store.Agent, error) {
	var released store.Agent
	err := m.server.store.WithTx(ctx, func(tx context.Context) error {
		confirmed, e := m.server.store.ConfirmAgentStage(tx, a.UID, a.Revision, a.OperationID, a.Stage, "absent")
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

func (m *provider) probe(ctx context.Context, a store.Agent) bool {
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

func (m *provider) actorByName(ctx context.Context, swarm, name string) (store.Agent, error) {
	actors, err := m.server.store.ListAgents(ctx, swarm)
	if err != nil {
		return store.Agent{}, err
	}
	for _, a := range actors {
		if a.Name == name && a.Phase != "deleted" {
			return a, nil
		}
	}
	return store.Agent{}, store.ErrNotFound
}
