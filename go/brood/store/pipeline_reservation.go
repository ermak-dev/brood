package store

import "context"

// Нативное создание актора под посещение узла закрыто (#208): узел называет
// роль, рой связывает её с существующим актором, а шаг адресует этого актора
// (AppendPipelineVisit проверяет привязку в транзакции записи). Протокол
// резервирования, привязки UID и неопределённого исхода оставлен входами,
// которые отказывают до любого чтения и записи; неопределённость шага
// inflight фиксирует RecoverPipelineActions. Колонки native_* в swarm_steps
// и чтение native_actor_name остаются, пока схема не сменится.

// PipelineNativeReservation identified a native actor creation request for a
// node visit. Creation is closed (#208); the type remains the argument of the
// closed entry points below.
type PipelineNativeReservation struct {
	SwarmID, RunID, NodeID, RequestID         string
	Sequence                                  int64
	TemplateName, TemplateUID, TemplateDigest string
	ActorName, State                          string
	Replay                                    bool
}

// ReservePipelineNativeActor is closed: it authorized creating a new native
// actor for a node visit, and a workflow node no longer gets an actor of its
// own. It returns ErrPipelineNodeActorCreation before any read or write and
// never authorizes creation.
func (s *Store) ReservePipelineNativeActor(_ context.Context, r PipelineNativeReservation) (PipelineNativeReservation, error) {
	r.ActorName, r.State, r.Replay = "", "", false
	return r, ErrPipelineNodeActorCreation
}

// BindPipelineNativeActor is closed: it wrote the UID of a natively created
// actor into a step, bypassing the role binding. It returns
// ErrPipelineNodeActorCreation before any read or write.
func (s *Store) BindPipelineNativeActor(_ context.Context, _ PipelineNativeReservation, _ string, _ int64) error {
	return ErrPipelineNodeActorCreation
}

// MarkPipelineNativeActorUncertain is closed together with native creation.
// It returns ErrPipelineNodeActorCreation before any read or write;
// RecoverPipelineActions marks any inflight step unconfirmed.
func (s *Store) MarkPipelineNativeActorUncertain(_ context.Context, _ PipelineNativeReservation) error {
	return ErrPipelineNodeActorCreation
}
