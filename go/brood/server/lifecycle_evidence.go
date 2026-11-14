package server

import (
	"context"

	"brood.dev/go/brood/command"
)

// lifecycleIntentEvidence описывает только новое намерение текущего вызова.
// Нулевое значение неизвестно: отсутствие инструментации не доказывает отказ.
type lifecycleIntentEvidence uint8

const (
	lifecycleIntentUnknown lifecycleIntentEvidence = iota
	lifecycleNoIntent
	lifecycleIntentPossible
	lifecycleIntentAccepted
)

type lifecycleEvidenceKey struct{}
type lifecycleExecution struct{ intent lifecycleIntentEvidence }

// executeOperatorCommandWithEvidence отделяет доказательство намерения от
// результата команды: ошибка последующей записи UPID может стереть actor,
// но не отменяет уже записанное намерение и отправленный запрос провайдеру.
// Коллектор принадлежит одному синхронному вызову, не хранится в Server.
func (s *Server) executeOperatorCommandWithEvidence(ctx context.Context, userID, swarmID, caller string, inv command.Invocation) (command.Result, lifecycleIntentEvidence, error) {
	execution := &lifecycleExecution{intent: lifecycleIntentUnknown}
	switch inv.Name {
	case "add", "delete", "stop", "start", "reset", "update":
		execution.intent = lifecycleNoIntent
	}
	ctx = context.WithValue(ctx, lifecycleEvidenceKey{}, execution)
	result, err := s.executeOperatorCommand(ctx, userID, swarmID, caller, inv)
	return result, execution.intent, err
}

// noteLifecycleIntent вызывается у записи намерения, до любого эффекта PVE.
// Possible отмечает подготовленный commit: его ошибка не доказывает rollback.
func noteLifecycleIntent(ctx context.Context, evidence lifecycleIntentEvidence) {
	if execution, ok := ctx.Value(lifecycleEvidenceKey{}).(*lifecycleExecution); ok {
		execution.intent = evidence
	}
}
