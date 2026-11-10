package server

// FEATURE: Доставка инструкции узла привязанному актору первым сообщением
// шага (#212).
// SPEC: Вход в actor-узел — запуск роя, ответ оператора, исход прошлого
// шага, старт сервера — будит диспетчер роя. Он резервирует попытку
// (store.ReserveActorStep: привязанный актор, инкарнация, request_id,
// workspace-lease), затем вне транзакции выдаёт challenge, перепроверяет,
// что шаг всё ещё attesting этой попытки (store.CheckActorStepAttestation),
// зовёт у гостя workspace_attest на commit источника, принимает только ответ
// с challenge этой попытки, проверяет подпись (attest.Registry) и записывает
// подтверждение (store.RecordActorStepAttestation), и только потом
// доставляет инструкцию каналом send (deliverToActor). Challenge живёт один
// вызов гостя. Каждый исход пишется условной записью хранилища: неизвестный
// — unconfirmed без повтора, заведомо неотправленный — refused с причиной;
// выключение сервера до вызова гостя возвращает шаг в pending
// (store.WithdrawActorStep).
// IMPLEMENTATION: один экземпляр сервера; диспетчер сериализует рой в
// процессе и сливает повторные пробуждения, но корректность держат
// условные UPDATE хранилища: параллельный проход той же попытки ничего не
// пишет и ничего не отправляет.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

const (
	// stepAttestationTTL — срок challenge: бюджет workspace_attest 10 минут
	// и запас на ответ (docs/PROXMOX.md, «Подтверждение workspace»).
	stepAttestationTTL = 15 * time.Minute
	// stepAttestationLimit — сколько challenge ждут ответа одновременно.
	stepAttestationLimit = 64
	// stepConnectWait — сколько ждать relay актора: после рестарта сервера
	// гости переподключаются с паузой до 5 секунд.
	stepConnectWait = time.Minute
	// stepBookkeepingTimeout — запись исхода после эффекта у гостя идёт и
	// при отмене сервера: иначе исход остался бы до восстановления на старте.
	stepBookkeepingTimeout = 5 * time.Second
)

// guestTextBytes — сколько текста отказа гостя попадает в причину шага.
const guestTextBytes = 512

// stepDispatcher доставляет инструкции шагов actor-узлов. Нулевое значение
// не годится: newStepDispatcher.
type stepDispatcher struct {
	server *Server
	// attestations выдаёт и сжигает challenge подтверждений; в памяти
	// процесса: рестарт отвергает все прежние ответы.
	attestations *attest.Registry
	connectWait  time.Duration

	mu      sync.Mutex
	ctx     context.Context
	closed  bool
	running map[string]bool
	again   map[string]bool
	wg      sync.WaitGroup
}

func newStepDispatcher(s *Server) *stepDispatcher {
	return &stepDispatcher{server: s, attestations: attest.NewRegistry(stepAttestationTTL, stepAttestationLimit), connectWait: stepConnectWait, running: map[string]bool{}, again: map[string]bool{}}
}

// start привязывает диспетчер ко времени жизни сервера и будит рои, чей шаг
// ждёт доставки. Попытки с неизвестным исходом newFromStore уже перевёл в
// unconfirmed (store.RecoverActorSteps) до старта.
func (d *stepDispatcher) start(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
	if err := d.kickPending(ctx); err != nil {
		slog.Error("actor step dispatch at start failed", "err", err.Error())
	}
}

// kickPending будит каждый рой, чей последний шаг ждёт доставки.
func (d *stepDispatcher) kickPending(ctx context.Context) error {
	swarms, err := d.server.store.PendingActorStepSwarms(ctx)
	if err != nil {
		return err
	}
	for _, id := range swarms {
		d.kick(id)
	}
	return nil
}

// kick будит диспетчер роя и не ждёт его. Рой обрабатывается одной
// горутиной; пробуждение во время прохода повторяет проход после него.
func (d *stepDispatcher) kick(swarmID string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx == nil || d.closed || d.ctx.Err() != nil {
		return
	}
	if d.running[swarmID] {
		d.again[swarmID] = true
		return
	}
	d.running[swarmID] = true
	d.wg.Add(1)
	go d.run(d.ctx, swarmID)
}

func (d *stepDispatcher) run(ctx context.Context, swarmID string) {
	defer d.wg.Done()
	for {
		d.dispatch(ctx, swarmID)
		d.mu.Lock()
		if !d.again[swarmID] || ctx.Err() != nil {
			delete(d.running, swarmID)
			delete(d.again, swarmID)
			d.mu.Unlock()
			return
		}
		delete(d.again, swarmID)
		d.mu.Unlock()
	}
}

// close перестаёт принимать пробуждения и ждёт идущие проходы; их контекст
// отменён сервером раньше.
func (d *stepDispatcher) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.wg.Wait()
}

// wait ждёт идущие проходы, не закрывая диспетчер.
func (d *stepDispatcher) wait() { d.wg.Wait() }

// dispatch — один проход роя: резерв, подтверждение, доставка. Повтор
// безопасен: без pending-шага резерв ничего не пишет.
func (d *stepDispatcher) dispatch(ctx context.Context, swarmID string) {
	s := d.server
	attempt, err := s.store.ReserveActorStep(ctx, swarmID, store.NewID(), stepMessageSender(swarmID))
	switch {
	case errors.Is(err, store.ErrNoActorStep):
		return
	case errors.Is(err, store.ErrActorStepRefused):
		slog.Info("actor step refused", "swarm", swarmID, "reason", err.Error())
		return
	case err != nil:
		slog.Error("actor step reservation failed", "swarm", swarmID, "err", err.Error())
		return
	}
	// Инструкция собирается до подтверждения: неотправляемый текст узла не
	// должен стоить актору остановки и перезапуска.
	text, err := stepInstruction(attempt)
	if err != nil {
		d.settle(ctx, attempt, false, "the node instruction cannot be delivered: "+err.Error())
		d.finishLease(ctx, attempt, true)
		return
	}
	if !d.attest(ctx, attempt) {
		return
	}
	d.deliver(ctx, attempt, text)
}

// finishLease пишет исход workspace-lease подтверждения: известный исход
// гостя отпускает его, неизвестный оставляет неподтверждённым (#201).
func (d *stepDispatcher) finishLease(ctx context.Context, a store.ActorStepAttempt, confirmed bool) {
	bctx, cancel := bookkeeping(ctx)
	defer cancel()
	if err := d.server.store.FinishActorOperation(bctx, a.Lease, confirmed); err != nil {
		slog.Error("workspace attestation lease was not settled", "swarm", a.SwarmID, "request", a.RequestID, "confirmed", confirmed, "err", err.Error())
	}
}

// attest подтверждает workspace привязанного актора на commit источника и
// записывает подтверждение. false — шаг закрыт (refused, unconfirmed),
// возвращён в pending или уже не этой попытки; инструкция не уходит.
func (d *stepDispatcher) attest(ctx context.Context, a store.ActorStepAttempt) bool {
	s := d.server
	// Исход workspace-lease пишется ровно один раз: известный исход гостя
	// (или гостя не звали) отпускает его, неизвестный оставляет
	// неподтверждённым; возврат шага в pending отпускает его сам.
	confirmed, withdrawn := true, false
	defer func() {
		if !withdrawn {
			d.finishLease(ctx, a, confirmed)
		}
	}()
	refuse := func(detail string) bool {
		d.settle(ctx, a, false, detail)
		return false
	}
	// Гостя не звали, а сервер выключается: шаг не закрывается, а
	// возвращается следующему диспетчеру (старт сервера) целиком.
	withdraw := func() bool {
		withdrawn = d.withdraw(ctx, a)
		return false
	}
	hub, ok := s.managed.hub.(sessionRelay)
	if !ok || s.managed.registry == nil || len(s.managed.registry.relaySecret) < 32 {
		return refuse("workspace attestation is unavailable: the Proxmox relay is not configured")
	}
	if !d.awaitConnection(ctx, a.Actor) {
		if ctx.Err() != nil {
			return withdraw()
		}
		return refuse("actor " + a.Actor.Name + " is not connected to the relay; no instruction was sent")
	}
	claim, err := d.attestations.Issue(attest.Request{ActorUID: a.Actor.UID, Generation: a.Actor.Incarnation, Swarm: a.SwarmID, Step: a.RequestID, Origin: a.Source.Origin, Commit: a.Source.Commit})
	if err != nil {
		return refuse("workspace attestation challenge unavailable: " + err.Error())
	}
	// Challenge живёт ровно один вызов гостя: отказ, мусор, чужой challenge
	// и неизвестный исход его сжигают, а не оставляют до истечения срока —
	// поздний ответ этой попытки не примет и следующая попытка.
	defer d.attestations.Revoke(claim.Challenge)
	args, err := json.Marshal(map[string]string{"swarm": claim.Swarm, "step": claim.Step, "origin": claim.Origin, "commit": claim.Commit, "challenge": claim.Challenge})
	if err != nil {
		return refuse("workspace attestation request encoding failed")
	}
	// Ожидание relay длится до минуты: рой могли остановить, а роль
	// перевязать. workspace_attest останавливает процессы актора и
	// перезапускает сессию, поэтому гостя зовут, только пока шаг всё ещё
	// attesting этой попытки.
	if err := s.store.CheckActorStepAttestation(ctx, a); err != nil || ctx.Err() != nil {
		if ctx.Err() != nil {
			return withdraw()
		}
		reason := "the step could not be checked"
		if stepChanged(err) {
			reason = err.Error()
		}
		return refuse("the step changed before workspace attestation (" + reason + "); no instruction was sent")
	}
	resp, err := hub.CallSession(ctx, a.Actor.UID, a.Actor.Incarnation, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: args, Caller: stepMessageSender(a.SwarmID)})
	if err != nil || resp.Outcome == "unknown" || resp.Outcome == "unconfirmed" {
		confirmed = false
		d.settle(ctx, a, true, "workspace attestation outcome is unknown; workspace operation "+a.RequestID+" stays reserved until an operator releases it; no instruction was sent")
		return false
	}
	if !resp.OK {
		return refuse("workspace attestation refused by the actor: " + guestText(resp.Text))
	}
	got, err := attest.DecodeAttestation([]byte(resp.Text))
	if err != nil {
		return refuse("workspace attestation rejected: " + err.Error())
	}
	// Consume ищет claim по присланному challenge: без этой сверки
	// подписанный ответ другой попытки (потерянный ответ прошлой, другой
	// инкарнации) открыл бы эту попытку.
	if got.Challenge != claim.Challenge {
		return refuse("workspace attestation rejected: the answer is for another attempt")
	}
	verified, err := d.attestations.Consume(got, workspaceAttestationKey(s.managed.registry.relaySecret))
	if err != nil {
		return refuse("workspace attestation rejected: " + err.Error())
	}
	if err := s.store.RecordActorStepAttestation(ctx, a, verified.Origin, verified.Commit); err != nil {
		// Остановленный рой уже закрыл шаг: условная запись отказа ничего
		// не поменяет. Иначе роль или источник разошлись с попыткой.
		return refuse("workspace attestation was not recorded: " + err.Error())
	}
	return true
}

// deliver отправляет инструкцию узла привязанному актору той же инкарнации.
// Перед отправкой и сразу после неё шаг обязан быть delivering этой попытки
// при идущем рое (store.CheckActorStepDelivery).
func (d *stepDispatcher) deliver(ctx context.Context, a store.ActorStepAttempt, text string) {
	s := d.server
	dctx, cancel := context.WithTimeout(ctx, requestDeliveryTimeout)
	defer cancel()
	bound := func(ctx context.Context) (store.ManagedActor, error) {
		current, err := s.store.GetManagedActor(ctx, a.Actor.UID)
		if err != nil || current.Incarnation != a.Actor.Incarnation {
			return store.ManagedActor{}, store.ErrNotFound
		}
		return current, nil
	}
	check := func(ctx context.Context) error { return s.store.CheckActorStepDelivery(ctx, a) }
	result, sent, err := s.deliverToActor(dctx, a.SwarmID, bound, relay.MessageRequest{From: stepMessageSender(a.SwarmID), Body: text}, check)
	switch {
	case !sent && ctx.Err() != nil:
		// Выключение рвёт чтения канала до отправки: причина — оно, а не
		// мнимая смена актора.
		d.settle(ctx, a, false, "instruction not sent: the server stopped before the send")
	case !sent:
		d.settle(ctx, a, false, "instruction not sent: "+stepDeliveryReason(result, err))
	case err == nil && result.Outcome == "completed":
		bctx, cancel := bookkeeping(ctx)
		defer cancel()
		if err := s.store.FinishActorStepDelivery(bctx, a); err != nil && !errors.Is(err, store.ErrPipelineVisitConflict) {
			slog.Error("instruction delivery was not recorded", "swarm", a.SwarmID, "request", a.RequestID, "err", err.Error())
		}
	default:
		d.settle(ctx, a, true, "instruction delivery outcome is unknown ("+stepDeliveryReason(result, err)+"); it is not resent: stop the swarm to resolve")
	}
}

// stepDeliveryReason — причина для оператора в шаге. Ошибки хранилища о
// попытке и привязке роли называют, что изменилось; канал — фиксированным
// текстом без подробностей транспорта.
func stepDeliveryReason(result command.Result, err error) string {
	switch {
	case err == nil:
		if result.Text != "" {
			return "the actor did not confirm receipt: " + guestText(result.Text)
		}
		return "the actor did not confirm receipt"
	case errors.Is(err, errRecipientOffline):
		return "the actor is not connected to the relay"
	case errors.Is(err, app.ErrNotFound):
		return "the bound actor is no longer this incarnation in the swarm"
	case errors.Is(err, errDeliveryUnconfirmed):
		return "the relay channel failed during the send"
	case errors.Is(err, app.ErrNotAllowed):
		return "the step or its actor changed during the send"
	case stepChanged(err):
		return err.Error()
	default:
		return "delivery unavailable"
	}
}

// stepChanged — ошибка хранилища говорит, что изменилось в попытке или
// привязке роли: её текст годится в причину шага.
func stepChanged(err error) bool {
	return errors.Is(err, store.ErrPipelineVisitConflict) || errors.Is(err, store.ErrSwarmRoleMismatch) || errors.Is(err, store.ErrSwarmRoleStale) ||
		errors.Is(err, store.ErrSwarmRoleInactive) || errors.Is(err, store.ErrSwarmRoleUnbound) || errors.Is(err, store.ErrSwarmRoleForeignActor)
}

// withdraw возвращает в pending попытку, до гостя не дошедшую, и отпускает
// её lease (store.WithdrawActorStep). false — шаг уже не этой попытки
// (остановлен) или запись не прошла: lease тогда отпускает attest.
func (d *stepDispatcher) withdraw(ctx context.Context, a store.ActorStepAttempt) bool {
	bctx, cancel := bookkeeping(ctx)
	defer cancel()
	err := d.server.store.WithdrawActorStep(bctx, a)
	switch {
	case err == nil:
		slog.Info("actor step returned to pending: the server stopped before the guest call", "swarm", a.SwarmID, "sequence", a.Sequence)
		return true
	case !errors.Is(err, store.ErrPipelineVisitConflict):
		slog.Error("actor step was not returned to pending", "swarm", a.SwarmID, "request", a.RequestID, "err", err.Error())
	}
	return false
}

// settle закрывает попытку: unknown — unconfirmed, иначе refused. Запись
// условна: шаг, который уже не этой попытки (остановлен, завершён), не
// меняется.
func (d *stepDispatcher) settle(ctx context.Context, a store.ActorStepAttempt, unknown bool, detail string) {
	bctx, cancel := bookkeeping(ctx)
	defer cancel()
	record := d.server.store.RefuseActorStep
	if unknown {
		record = d.server.store.UnconfirmActorStep
	}
	err := record(bctx, a, detail)
	switch {
	case err == nil:
		slog.Info("actor step settled", "swarm", a.SwarmID, "sequence", a.Sequence, "unknown", unknown, "reason", detail)
	case !errors.Is(err, store.ErrPipelineVisitConflict):
		slog.Error("actor step outcome was not recorded", "swarm", a.SwarmID, "request", a.RequestID, "err", err.Error())
	}
}

// awaitConnection ждёт relay актора не дольше connectWait.
func (d *stepDispatcher) awaitConnection(ctx context.Context, a store.ManagedActor) bool {
	hub := d.server.managed.hub
	if hub == nil {
		return false
	}
	deadline := time.Now().Add(d.connectWait)
	for {
		if hub.Connected(a.UID, a.Incarnation) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func bookkeeping(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stepBookkeepingTimeout)
}

// guestText — отказ гостя для причины шага: одна строка ограниченной длины.
func guestText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "no reason given"
	}
	return excerpt(text, guestTextBytes)
}
