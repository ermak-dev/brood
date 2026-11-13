package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// guestReleaseTimeout — бюджет чтения набора у гостя: root читает одну
// ссылку, и занятый relay не должен задерживать ответ update.
const guestReleaseTimeout = 5 * time.Second

// reportedRelease — то, что root гостя сообщил о своём наборе (#133).
type reportedRelease struct {
	// id — набор releases/<id>, на который указывает bin.
	id string
	// plain — bin гостя плоский каталог bootstrap до #184.
	plain bool
	// unknown — почему набор неизвестен; "" у сообщённого набора.
	unknown string
}

// guestRelease спрашивает root гостя a, на какой набор указывает его
// /opt/brood/bin (relay.GuestReleaseOperation). Гость без relay, runtime до
// #133 (unsupported), отказ чтения и любой неожиданный ответ дают неизвестный
// набор с причиной, без догадки. Причина — фиксированная фраза: текст гостя
// в отказ не попадает.
func (m *managedProvider) guestRelease(ctx context.Context, a store.ManagedActor) reportedRelease {
	hub, ok := m.hub.(sessionRelay)
	if !ok || !m.hub.Connected(a.UID, a.Incarnation) {
		return reportedRelease{unknown: "its guest relay is not connected"}
	}
	ctx, cancel := context.WithTimeout(ctx, guestReleaseTimeout)
	defer cancel()
	reply, err := hub.CallSession(ctx, a.UID, a.Incarnation, relay.SessionRequest{Operation: relay.GuestReleaseOperation, Arguments: json.RawMessage(`{}`), Caller: "server:update"})
	switch {
	case err != nil:
		return reportedRelease{unknown: "the guest relay call failed"}
	case reply.Outcome == "unsupported":
		return reportedRelease{unknown: "its guest runtime predates guest_release"}
	case !reply.OK || reply.Outcome != "complete":
		return reportedRelease{unknown: "the guest did not confirm it"}
	}
	var answer relay.GuestRelease
	decoder := json.NewDecoder(bytes.NewReader([]byte(reply.Text)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&answer) != nil || decoder.Decode(new(any)) != io.EOF {
		return reportedRelease{unknown: "the guest answer is malformed"}
	}
	switch {
	case answer.Layout == "release" && guestArtifactDigestPattern.MatchString(answer.Release):
		return reportedRelease{id: answer.Release}
	case answer.Layout == "plain" && answer.Release == "":
		return reportedRelease{plain: true}
	}
	return reportedRelease{unknown: "the guest answer is malformed"}
}

// managedRunning: актор работает и согласован, его start завершён.
func managedRunning(a store.ManagedActor) bool {
	return a.Phase == "running" && a.Desired == "running" && a.Operation == "start" && a.Stage == "done"
}

// updateActor выполняет update (#133, вариант оператора 1): контролируемый
// перезапуск работающего актора в его CT, после которого bootstrap #184
// ставит набор гостевых программ, который сервер раздаёт сейчас. CTID, диск,
// owner nonce, поколение, шаблон и ресурсы профиля сохраняются; reset не
// участвует. Полномочия проверил вызывающий, как для stop/start (#152).
func (s *Server) updateActor(ctx context.Context, userID string, sw app.Swarm, pool store.ManagedPool, a store.ManagedActor, parent *store.ManagedActor) (command.Result, error) {
	// Остановленного актора update не поднимает: намерение stopped меняет
	// только явный start, и он сам ставит текущий набор. Иначе update был
	// бы start под другим именем.
	if !managedRunning(a) {
		// Start, stop или delete, ждущие запроса, называются как есть:
		// задачи нет (#133, #275, #277).
		if notRequested(a) {
			return command.Result{}, actorCommandError(newWaitingRefusal("update", a))
		}
		// Незавершённое изменение (в том числе сам перезапуск update, чья
		// CT уже остановлена) start не заменит: совет start был бы ложным.
		if a.Stage != "done" {
			return command.Result{}, command.Refusef(app.ErrConflict, "update refused: actor %q is %s while its last lifecycle change is unfinished; update restarts only a running, reconciled actor", a.Name, a.Phase)
		}
		if a.Desired == "stopped" || a.Phase == "stopped" {
			return command.Result{}, command.Refusef(app.ErrConflict, "update refused: actor %q is stopped; update restarts a running actor; start it with start, which installs the server's current guest release", a.Name)
		}
		return command.Result{}, command.Refusef(app.ErrConflict, "update refused: actor %q is %s, not running; update restarts only a running, reconciled actor", a.Name, a.Phase)
	}
	// Шаг в работе, аренда и landing — до вызова гостя: занятому актору
	// отказ без побочных запросов. Намерение повторит проверку в своей
	// транзакции.
	if err := s.store.CheckActorUpdate(ctx, a); err != nil {
		return command.Result{}, actorCommandError(err)
	}
	// Без проверенного манифеста bootstrap #184 откажет fail closed, и
	// перезапуск оставил бы актора остановленным.
	target, ok := s.currentGuestRelease()
	if !ok {
		return command.Result{}, command.Refusef(app.ErrConflict, "update refused: the server has no verified guest release (%s); a restart would leave the actor stopped", guestArtifactsSetting)
	}
	before := s.managed.guestRelease(ctx, a)
	if before.unknown != "" {
		// Неизвестный набор — отказ, а не перезапуск наугад. Runtime до #133
		// отвечает unsupported и не отличает CT с bootstrap #184 от CT
		// прежнего шаблона, а её перезапуск после выкладки с другими
		// артефактами оставил бы актора остановленным навсегда (#184).
		// Такой runtime отдаёт guest_release UID 1000 helper: подложный ответ
		// актора без набора тоже даёт отказ, а названный набор доказательством
		// root не является, и перезапуск тогда затрагивает только CT самого
		// актора.
		return command.Result{}, command.Refusef(app.ErrConflict, "update refused: actor %q did not report its current guest release (%s), and update does not restart a container whose release it cannot check; a guest runtime older than #133 cannot report it: a one-time stop and start installs one that can, but only in a container created from a template with the bootstrap of #184; a container from an earlier template must not be stopped or restarted after a deploy that changed the guest artifacts (docs/PROXMOX.md)", a.Name, before.unknown)
	}
	if before.plain {
		// Плоский bin — bootstrap шаблона до #184: старт ставит артефакты
		// поверх и отказывает при другом наборе, CT больше не стартует.
		return command.Result{}, command.Refusef(app.ErrConflict, "update refused: actor %q runs a guest bootstrap before guest releases (#184): its /opt/brood/bin is a plain directory, so a restart cannot install release %s and fails once the artifacts change; recreate the actor in a pool on a current template", a.Name, target)
	}
	if before.id == target {
		// Уже на наборе сервера: перезапуск ничего бы не обновил. Повтор
		// update безопасен и не прерывает работу; намеренный перезапуск CT —
		// stop и start.
		return updateResult(a, target, target, "completed", fmt.Sprintf("actor %q already runs guest release %s, the server's current release; nothing was restarted; the profile's template and resources do not change", a.Name, target)), nil
	}
	op := &managedWorkloadOperation{server: s, context: ctx, userID: userID, swarm: sw, pool: pool, name: a.Name, action: "update", actor: a, parent: parent, operationID: store.NewID()}
	err := s.admitWorkload(userID, sw.ID, op)
	saved := op.saved
	if err != nil {
		// Отказ мог случиться и после записи намерения: PVE принял остановку,
		// а запись её UPID не удалась, или не записалась отметка отправки
		// Stop. Ответ называет состояние базы, а не копию в памяти.
		// Записанное намерение — неизвестный исход или ждущий Stop, иначе —
		// обычный отказ.
		current, getErr := s.store.GetManagedActor(context.WithoutCancel(ctx), a.UID)
		if getErr == nil && current.OperationID != op.operationID {
			return command.Result{}, actorCommandError(err)
		}
		saved = current
		if getErr != nil {
			// Состояние не прочитать: ответ называет только намерение.
			saved = store.ManagedActor{UID: a.UID, Name: a.Name, Incarnation: a.Incarnation, OperationID: op.operationID}
		}
	}
	// Stop, отказавший до записи (#275) или не дошедший до отметки отправки
	// (#277), PVE не получал: намерение записано, и reconcile запрашивает
	// остановку каждым проходом. Это принятие, а не неизвестный исход, даже
	// если сам запрос кончился ошибкой.
	if stopNotRequested(saved) {
		last := ""
		if detail := saved.Diagnostic(); detail != "" {
			last = " (last attempt failed: " + detail + ")"
		}
		return updateResult(saved, before.id, target, "accepted", fmt.Sprintf("actor %q update accepted (uid %s): a controlled restart in the retained container from guest release %s to %s; the profile's template and resources do not change; its stop is not yet requested%s; reconcile requests it on every pass; the restart is asynchronous, not a completion or readiness confirmation; current state: %s", a.Name, a.UID, before.id, target, last, saved.Phase)), nil
	}
	// Иначе успех — только принятая PVE задача остановки: UPID записан,
	// ошибки нет. Без UPID или с ошибкой исход неизвестен: задача могла
	// начаться, поэтому это не принятие и не повод повторять.
	if err != nil || saved.Stage != "stop" || saved.UPID == "" || saved.Phase == "error" {
		detail := saved.Diagnostic()
		switch {
		case detail != "":
		case err != nil:
			detail = "the stop task was not recorded"
		default:
			detail = "no task was confirmed"
		}
		result := updateResult(saved, before.id, target, "unknown", "")
		return result, fmt.Errorf("actor %q update outcome is unknown: the stop of the controlled restart from guest release %s to %s has no confirmed and recorded Proxmox task (%s); intent %s is retained and is not retried automatically; reconcile the allocation before another lifecycle change", a.Name, before.id, target, detail, saved.OperationID)
	}
	return updateResult(saved, before.id, target, "accepted", fmt.Sprintf("actor %q update accepted (uid %s): a controlled restart in the retained container from guest release %s to %s; the profile's template and resources do not change; the restart is asynchronous, not a completion or readiness confirmation; current state: %s", a.Name, a.UID, before.id, target, saved.Phase)), nil
}

func updateResult(a store.ManagedActor, before, after, outcome, text string) command.Result {
	data, _ := json.Marshal(struct {
		UID           string `json:"uid"`
		Name          string `json:"name"`
		Generation    int64  `json:"generation"`
		OperationID   string `json:"operation_id"`
		Operation     string `json:"operation"`
		Stage         string `json:"stage"`
		State         string `json:"state"`
		Desired       string `json:"desired"`
		ReleaseBefore string `json:"release_before"`
		ReleaseAfter  string `json:"release_after"`
	}{a.UID, a.Name, a.Incarnation, a.OperationID, a.Operation, a.Stage, a.Phase, a.Desired, before, after})
	return command.Result{Outcome: outcome, Text: text, Data: data}
}
