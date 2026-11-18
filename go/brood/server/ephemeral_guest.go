package server

import (
	"context"
	"encoding/json"
	"strings"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type childSpawnArguments struct {
	Sequence        int64  `json:"parent_sequence"`
	ParentRequestID string `json:"parent_request_id"`
	RequestID       string `json:"request_id"`
	Task            string `json:"task"`
	Total           int    `json:"total"`
	PoolUID         string `json:"pool_uid"`
	JobID           string `json:"job_id"`
}

// ephemeralJobJSON содержит только сохранённые публичные факты. queued_at —
// факт приёма, не вычисленное состояние очереди или обещание physical success.
type ephemeralJobJSON struct {
	JobID           string                         `json:"job_id"`
	ParentSequence  int64                          `json:"parent_sequence"`
	ParentRequestID string                         `json:"parent_request_id"`
	Pass            int64                          `json:"pass"`
	RequestID       string                         `json:"request_id"`
	Task            string                         `json:"task"`
	PoolUID         string                         `json:"pool_uid"`
	QueuedAt        string                         `json:"queued_at"`
	ChildID         string                         `json:"child_id,omitempty"`
	Failure         *passTraceEphemeralFailureJSON `json:"failure,omitempty"`
	ExecutorFailure *guestExecutorFailureJSON      `json:"executor_failure,omitempty"`
}

func ephemeralJobView(history store.EphemeralChildHistory) ephemeralJobJSON {
	intent := history.Intent
	result := ephemeralJobJSON{JobID: intent.ID, ParentSequence: intent.Parent.Sequence, ParentRequestID: intent.Parent.RequestID, Pass: intent.Parent.Pass, RequestID: intent.RequestID, Task: intent.Payload, PoolUID: intent.PoolUID, QueuedAt: intent.CreatedAt, ChildID: history.ChildID}
	if failure := history.Failure; failure != nil {
		result.Failure = &passTraceEphemeralFailureJSON{Reason: failure.Reason, FailedAt: failure.FailedAt}
	}
	result.ExecutorFailure = guestExecutorFailureView(history.ExecutorFailure)
	return result
}

func (m *managedProvider) spawnChild(ctx context.Context, from store.ManagedActor, args childSpawnArguments) (store.EphemeralChildIntent, error) {
	if strings.Contains(args.Task, envelopeClosingTag) {
		return store.EphemeralChildIntent{}, store.ErrSwarmChildConflict
	}
	var intent store.EphemeralChildIntent
	err := m.withChildParent(ctx, from, args.Sequence, args.ParentRequestID, func(ctx context.Context, p store.SwarmChildParent) error {
		policy, err := m.childNodePolicy(ctx, p, args.Total)
		if err != nil {
			return err
		}
		intent, err = m.server.store.EnqueueEphemeralChild(ctx, p, policy, args.RequestID, args.Task, args.PoolUID, m.childAuthority)
		return err
	})
	return intent, err
}

// authorizeEphemeralJobParent вызывается только внутри command actor transaction.
// GetSwarmChildJoin проверяет также текущие pass/request, назначение роли и шаг;
// одной masterhood недостаточно даже для чтения immutable intent.
func (m *managedProvider) authorizeEphemeralJobParent(ctx context.Context, current store.ManagedActor, intent store.EphemeralChildIntent) error {
	if childPrincipal(current, intent.Parent.Principal.ProjectID) != intent.Parent.Principal {
		return store.ErrSwarmChildDenied
	}
	_, err := m.server.store.GetSwarmChildJoin(ctx, intent.Parent, m.childAuthority)
	return err
}

func (m *managedProvider) ephemeralJobStatus(ctx context.Context, from store.ManagedActor, id string) (store.EphemeralChildHistory, error) {
	var history store.EphemeralChildHistory
	err := m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
		intent, err := m.server.store.GetEphemeralChildIntent(ctx, id)
		if err != nil {
			return err
		}
		if err = m.authorizeEphemeralJobParent(ctx, current, intent); err != nil {
			return err
		}
		history, err = m.server.store.GetEphemeralChildHistory(ctx, id)
		return err
	})
	if err != nil {
		return store.EphemeralChildHistory{}, err
	}
	// Повторная внешняя namespace-проверка не держит SQLite transaction. После неё
	// durable identity и родительская authority проверяются снова в общей транзакции.
	err = m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
		return m.authorizeEphemeralJobParent(ctx, current, history.Intent)
	})
	if err != nil {
		return store.EphemeralChildHistory{}, err
	}
	return history, nil
}

func (m *managedProvider) ephemeralJobCommand(ctx context.Context, from store.ManagedActor, inv command.Invocation) relay.SwarmResponse {
	validated, err := command.ParseTool(inv.Name, inv.Arguments)
	if err != nil {
		return relay.SwarmResponse{Text: "invalid child job arguments; check the command schema", Outcome: "refused"}
	}
	var args childSpawnArguments
	if err = json.Unmarshal(validated.Arguments, &args); err != nil {
		return childCommandRefusal(store.ErrSwarmChildConflict)
	}
	mutated := inv.Name == "child_spawn"
	if mutated {
		intent, err := m.spawnChild(ctx, from, args)
		if err != nil {
			return childCommandRefusal(err)
		}
		args.JobID = intent.ID
	}
	history, err := m.ephemeralJobStatus(ctx, from, args.JobID)
	if err != nil {
		if mutated {
			return childMutationUnconfirmed()
		}
		return childCommandRefusal(err)
	}
	outcome := "completed"
	if mutated {
		outcome = "accepted"
	}
	return childGuestResponse(outcome, "Stored ephemeral job facts only. Queuing is not child registration, delivery, an executor report or physical success; no aggregate job state is inferred. Exact retries return the original job, not new work.", ephemeralJobView(history))
}
