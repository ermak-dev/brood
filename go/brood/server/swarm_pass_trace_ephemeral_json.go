package server

import "brood.dev/go/brood/store"

type passTraceEphemeralJobJSON struct {
	ID                   string                            `json:"id"`
	RequestID            string                            `json:"requestId"`
	Task                 string                            `json:"task"`
	PoolUID              string                            `json:"poolUid"`
	QueuedAt             string                            `json:"queuedAt"`
	Source               swarmSourceJSON                   `json:"source"`
	Allocation           *passTraceEphemeralAllocationJSON `json:"allocation,omitempty"`
	CreateAuthorization  *passTraceEphemeralCreateJSON     `json:"createAuthorization,omitempty"`
	Attestation          *passTraceEphemeralProofJSON      `json:"attestation,omitempty"`
	ChildID              string                            `json:"childId,omitempty"`
	Inventory            *passTraceEphemeralInventoryJSON  `json:"inventory,omitempty"`
	InventoryUnavailable string                            `json:"inventoryUnavailable,omitempty"`
	ResultRetention      *passTraceEphemeralResultJSON     `json:"resultRetention,omitempty"`
	Failure              *passTraceEphemeralFailureJSON    `json:"failure,omitempty"`
	ExecutorFailure      *executorFailureJSON              `json:"executorFailure,omitempty"`
}

type passTraceEphemeralAllocationJSON struct {
	ActorUID         string `json:"actorUid"`
	ActorIncarnation int64  `json:"actorIncarnation"`
	AllocatedAt      string `json:"allocatedAt"`
}

type passTraceEphemeralCreateJSON struct {
	State      string `json:"state"`
	CreatedAt  string `json:"createdAt"`
	ReservedAt string `json:"reservedAt,omitempty"`
}

type passTraceEphemeralProofJSON struct {
	State          string `json:"state"`
	VerifiedOrigin string `json:"verifiedOrigin,omitempty"`
	VerifiedCommit string `json:"verifiedCommit,omitempty"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

type passTraceEphemeralInventoryJSON struct {
	UID         string `json:"uid"`
	Incarnation int64  `json:"incarnation"`
	CurrentName string `json:"currentName,omitempty"`
	Phase       string `json:"phase"`
	Desired     string `json:"desired"`
	Operation   string `json:"operation"`
	Stage       string `json:"stage"`
	UpdatedAt   string `json:"updatedAt"`
}

type passTraceEphemeralResultJSON struct {
	ChildID       string `json:"childId"`
	ChildRevision int64  `json:"childRevision"`
	Outcome       string `json:"outcome"`
	ParentOutcome string `json:"parentOutcome"`
	RetainedAt    string `json:"retainedAt"`
}

type passTraceEphemeralFailureJSON struct {
	Reason   string `json:"reason"`
	FailedAt string `json:"failedAt"`
}

type executorFailureJSON struct {
	IntentID  string `json:"intentId"`
	ChildID   string `json:"childId"`
	RequestID string `json:"requestId"`
	Reason    string `json:"reason"`
	FailedAt  string `json:"failedAt"`
}

func executorFailureView(f *store.EphemeralChildExecutorFailure) *executorFailureJSON {
	if f == nil {
		return nil
	}
	return &executorFailureJSON{IntentID: f.IntentID, ChildID: f.ChildID, RequestID: f.RequestID, Reason: f.Reason, FailedAt: f.FailedAt}
}

func childExecutorFailure(group store.SwarmChildJoin, childID string) *store.EphemeralChildExecutorFailure {
	for _, f := range group.ExecutorFailures {
		if f.ChildID == childID {
			return &f
		}
	}
	return nil
}

func passTraceEphemeralJobView(history store.EphemeralChildHistory) passTraceEphemeralJobJSON {
	intent := history.Intent
	result := passTraceEphemeralJobJSON{ID: intent.ID, RequestID: intent.RequestID, Task: intent.Payload, PoolUID: intent.PoolUID, QueuedAt: intent.CreatedAt, Source: swarmSourceJSON{Origin: intent.Origin, Commit: intent.SourceCommit}, ChildID: history.ChildID, InventoryUnavailable: history.InventoryUnavailable}
	if allocation := history.Allocation; allocation != nil {
		result.Allocation = &passTraceEphemeralAllocationJSON{ActorUID: allocation.ActorUID, ActorIncarnation: allocation.ActorIncarnation, AllocatedAt: allocation.CreatedAt}
	}
	if grant := history.CreateGrant; grant != nil {
		result.CreateAuthorization = &passTraceEphemeralCreateJSON{State: grant.State, CreatedAt: grant.CreatedAt, ReservedAt: grant.ReservedAt}
	}
	if proof := history.Attestation; proof != nil {
		result.Attestation = &passTraceEphemeralProofJSON{State: proof.State, VerifiedOrigin: proof.AttestedOrigin, VerifiedCommit: proof.AttestedCommit, CreatedAt: proof.CreatedAt, UpdatedAt: proof.UpdatedAt}
	}
	if inventory := history.Inventory; inventory != nil {
		result.Inventory = &passTraceEphemeralInventoryJSON{UID: inventory.UID, Incarnation: inventory.Incarnation, CurrentName: inventory.Name, Phase: inventory.Phase, Desired: inventory.Desired, Operation: inventory.Operation, Stage: inventory.Stage, UpdatedAt: inventory.UpdatedAt}
	}
	if seal := history.ResultSeal; seal != nil {
		result.ResultRetention = &passTraceEphemeralResultJSON{ChildID: seal.ChildID, ChildRevision: seal.ChildRevision, Outcome: seal.Outcome, ParentOutcome: seal.ParentOutcome, RetainedAt: seal.CreatedAt}
	}
	if failure := history.Failure; failure != nil {
		result.Failure = &passTraceEphemeralFailureJSON{Reason: failure.Reason, FailedAt: failure.FailedAt}
	}
	result.ExecutorFailure = executorFailureView(history.ExecutorFailure)
	return result
}
