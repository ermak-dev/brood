package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type guestExecutorFailureJSON struct {
	IntentID  string `json:"intent_id"`
	ChildID   string `json:"child_id"`
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`
	FailedAt  string `json:"failed_at"`
}

func guestExecutorFailureView(f *store.EphemeralChildExecutorFailure) *guestExecutorFailureJSON {
	if f == nil {
		return nil
	}
	return &guestExecutorFailureJSON{IntentID: f.IntentID, ChildID: f.ChildID, RequestID: f.RequestID, Reason: f.Reason, FailedAt: f.FailedAt}
}

type childTaskJSON struct {
	ID              string                    `json:"child_id"`
	RequestID       string                    `json:"request_id"`
	Role            string                    `json:"role"`
	State           string                    `json:"state"`
	Delivery        string                    `json:"delivery"`
	Detail          string                    `json:"delivery_detail,omitempty"`
	Outcome         string                    `json:"outcome,omitempty"`
	Report          string                    `json:"report,omitempty"`
	Task            string                    `json:"task,omitempty"`
	Revision        int64                     `json:"revision"`
	ExecutorFailure *guestExecutorFailureJSON `json:"executor_failure,omitempty"`
}

func childTaskView(c store.SwarmChildTask) childTaskJSON {
	return childTaskJSON{ID: c.ID, RequestID: c.RequestID, Role: c.Role, State: c.State, Delivery: c.Delivery, Detail: c.DeliveryDetail, Outcome: c.Outcome, Report: c.Report, Task: c.Payload, Revision: c.Revision}
}
func childGuestResponse(outcome, text string, data any) relay.SwarmResponse {
	raw, err := json.Marshal(data)
	if err != nil {
		return childCommandRefusal(err)
	}
	r := relay.SwarmResponse{OK: true, Outcome: outcome, Text: text, Data: raw}
	if outcome == "unconfirmed" || outcome == "failed" {
		r.OK = false
	}
	encoded, err := json.Marshal(r)
	if err != nil || len(encoded) > relay.MaxMessageBytes {
		return childCommandRefusal(errors.New("bounded response unavailable"))
	}
	return r
}

// childStatus exposes history only to the exact current parent/executor, with
// current scheduling authority and namespace checks before and after reading.
func (m *managedProvider) childStatus(ctx context.Context, from store.ManagedActor, id string) (store.SwarmChildTask, error) {
	task, _, err := m.childStatusSnapshot(ctx, from, id)
	return task, err
}

func (m *managedProvider) childStatusView(ctx context.Context, from store.ManagedActor, id string) (childTaskJSON, error) {
	task, failure, err := m.childStatusSnapshot(ctx, from, id)
	if err != nil {
		return childTaskJSON{}, err
	}
	result := childTaskView(task)
	result.ExecutorFailure = guestExecutorFailureView(failure)
	return result, nil
}

func (m *managedProvider) childStatusSnapshot(ctx context.Context, from store.ManagedActor, id string) (store.SwarmChildTask, *store.EphemeralChildExecutorFailure, error) {
	var w store.SwarmChildWork
	var failure *store.EphemeralChildExecutorFailure
	err := m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
		var err error
		w, err = m.server.store.SwarmChildWorkByID(ctx, id)
		if err != nil {
			return err
		}
		principal := childPrincipal(current, w.Parent.Principal.ProjectID)
		if principal != w.Parent.Principal && principal != w.Task.Executor {
			return store.ErrSwarmChildDenied
		}
		if w.Task.Role == "" {
			intent, err := m.server.store.GetEphemeralChildIntentForTask(ctx, w.Task.ID)
			if err != nil || intent.Parent != w.Parent {
				return store.ErrSwarmChildDenied
			}
			fact, err := m.server.store.GetEphemeralChildExecutorFailure(ctx, intent.ID)
			if err == nil {
				if fact.ChildID != w.Task.ID || fact.RequestID != w.Task.RequestID {
					return store.ErrSwarmChildDenied
				}
				failure = &fact
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return m.childAuthority(ctx, store.SwarmChildAuthorization{Action: "read", Parent: w.Parent})
	})
	if err != nil {
		return store.SwarmChildTask{}, nil, err
	}
	if _, _, err := m.authorizeCommandActor(ctx, from); err != nil {
		return store.SwarmChildTask{}, nil, err
	}
	err = m.server.store.WithTx(ctx, func(ctx context.Context) error {
		if _, _, err := m.authorizeCommandActorDurable(ctx, from); err != nil {
			return err
		}
		return m.childAuthority(ctx, store.SwarmChildAuthorization{Action: "read", Parent: w.Parent})
	})
	if err != nil {
		return store.SwarmChildTask{}, nil, err
	}
	return w.Task, failure, nil
}

func (m *managedProvider) childCommand(ctx context.Context, from store.ManagedActor, inv command.Invocation) relay.SwarmResponse {
	validated, err := command.ParseTool(inv.Name, inv.Arguments)
	if err != nil {
		return relay.SwarmResponse{Text: "invalid child command arguments; check the command schema", Outcome: "refused"}
	}
	var args struct {
		childCreateArguments
		ChildID     string `json:"child_id"`
		Outcome     string `json:"outcome"`
		Report      string `json:"report"`
		WaitSeconds int    `json:"wait_seconds"`
	}
	if err := json.Unmarshal(validated.Arguments, &args); err != nil {
		return childCommandRefusal(store.ErrSwarmChildConflict)
	}
	switch inv.Name {
	case "child_create":
		c, err := m.createChild(ctx, from, args.childCreateArguments)
		if err != nil {
			return childCommandRefusal(err)
		}
		m.server.children.kick()
		status, err := m.childStatusView(ctx, from, c.ID)
		if err != nil {
			return childMutationUnconfirmed()
		}
		outcome := "accepted"
		if status.Delivery == "unconfirmed" || status.Delivery == "sending" {
			outcome = "unconfirmed"
		} else if status.Delivery == "not_sent" {
			outcome = "failed"
		}
		return childGuestResponse(outcome, fmt.Sprintf("Child %s: state=%s, delivery=%s. Durable intent only; exact retries never resend and delivery is not task completion.", status.ID, status.State, status.Delivery), status)
	case "child_get":
		status, err := m.childStatusView(ctx, from, args.ChildID)
		if err != nil {
			return childCommandRefusal(err)
		}
		return childGuestResponse("completed", fmt.Sprintf("Stored child status: %s, state=%s, delivery=%s. Query success is not a task completion claim.", status.ID, status.State, status.Delivery), status)
	case "child_cancel":
		c, err := m.cancelChild(ctx, from, args.Sequence, args.ParentRequestID, args.RequestID)
		if err != nil {
			return childCommandRefusal(err)
		}
		status, err := m.childStatusView(ctx, from, c.ID)
		if err != nil {
			return childMutationUnconfirmed()
		}
		m.server.children.kick()
		return childGuestResponse("completed", "Unsent child intent cancelled. No external effect was undone; other child obligations remain held.", status)
	case "child_complete":
		if err := m.completeChild(ctx, from, args.ChildID, args.RequestID, args.Outcome, args.Report); err != nil {
			return childCommandRefusal(err)
		}
		if _, _, err := m.authorizeCommandActor(ctx, from); err != nil {
			return childMutationUnconfirmed()
		}
		return childGuestResponse("completed", "The child outcome and independent report were recorded. The parent advances only through its server-computed join.", map[string]string{"child_id": args.ChildID, "state": "completed"})
	case "child_join":
		return m.waitChildJoin(ctx, from, args.Sequence, args.ParentRequestID, args.WaitSeconds)
	}
	return childCommandRefusal(store.ErrSwarmChildDenied)
}

func childMutationUnconfirmed() relay.SwarmResponse {
	return relay.SwarmResponse{Text: "The child operation may have been accepted, but authorization or availability changed before its result could be returned. Durable intents and possible effects are retained; retries never resend.", Outcome: "unconfirmed"}
}

func (m *managedProvider) waitChildJoin(ctx context.Context, from store.ManagedActor, sequence int64, request string, wait int) relay.SwarmResponse {
	waitCtx := ctx
	cancel := func() {}
	if wait > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(wait)*time.Second)
	}
	defer cancel()
	var last store.SwarmChildJoin
	for {
		j, err := m.childJoin(waitCtx, from, sequence, request, true)
		if err != nil {
			// A failed transaction/commit is not proof that the parent stayed open.
			// Never replace an ambiguous mutation with an older pending snapshot.
			return childCommandRefusal(err)
		}
		last = j
		joined := j.Evaluation.Open == 0 && !j.Interrupted && j.State != "aborted"
		if joined || j.Interrupted || j.State == "aborted" || wait == 0 {
			return childJoinResponse(j, joined)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return childCommandRefusal(ctx.Err())
			}
			return childJoinResponse(last, false)
		case <-timer.C:
		}
	}
}

func childJoinResponse(j store.SwarmChildJoin, joined bool) relay.SwarmResponse {
	joined = joined && !j.Interrupted && j.State != "aborted" && j.CompletionReceipt == nil
	// Reports and payloads are not multiplied into a potentially megabyte join
	// envelope. Each original is available, unchanged, through child_get.
	type childSummary struct {
		ID              string                    `json:"child_id"`
		RequestID       string                    `json:"request_id"`
		Role            string                    `json:"role"`
		State           string                    `json:"state"`
		Delivery        string                    `json:"delivery"`
		Outcome         string                    `json:"outcome,omitempty"`
		ExecutorFailure *guestExecutorFailureJSON `json:"executor_failure,omitempty"`
	}
	children := make([]childSummary, 0, len(j.Children))
	for _, c := range j.Children {
		children = append(children, childSummary{c.ID, c.RequestID, c.Role, c.State, c.Delivery, c.Outcome, guestExecutorFailureView(childExecutorFailure(j, c.ID))})
	}
	data := struct {
		ParentSequence int64          `json:"parent_sequence"`
		Pass           int64          `json:"pass"`
		State          string         `json:"state"`
		GroupState     string         `json:"group_state"`
		Mode           string         `json:"mode"`
		Total          int            `json:"total"`
		Open           int            `json:"open"`
		ExecutorFailed int            `json:"executor_failed"`
		Joined         bool           `json:"joined"`
		Interrupted    bool           `json:"interrupted"`
		Children       []childSummary `json:"children"`
	}{j.Parent.Sequence, j.Parent.Pass, j.Evaluation.State, j.State, j.Policy.Mode, j.Policy.Total, j.Evaluation.Open, len(j.ExecutorFailures), joined, j.Interrupted, children}
	outcome := "accepted"
	text := fmt.Sprintf("Child join snapshot readiness=%s, outstanding obligations=%d. No parent advancement is confirmed by this result; readiness never releases running work.", j.Evaluation.State, j.Evaluation.Open)
	if joined {
		outcome = "completed"
		text = "All child obligations settled; the server computed the parent outcome and bounded report and advanced its graph step."
	}
	if j.Interrupted {
		outcome = "unconfirmed"
		text = "Explicit owner intervention interrupted this execution. Child reports are retained as evidence, not proof of parent success; reconcile before automatic progression."
	}
	if j.State == "aborted" {
		text = "The incomplete unstarted group was explicitly aborted by an owner/admin. This is not a successful join and does not complete the parent."
	}
	response := childGuestResponse(outcome, text, data)
	if j.CompletionReceipt != nil {
		metadata, err := relay.WithCompletionReceipt(response.Data, completionReceiptMetadata(*j.CompletionReceipt))
		if err != nil {
			return childMutationUnconfirmed()
		}
		return childGuestResponse("accepted", "All child obligations settled; parent completion accepted, pending verified result evidence and policy.", metadata)
	}
	return response
}

// childCommandRefusal exposes only fixed typed public reasons, not SQL/provider
// errors or another actor's task/report. Cancellation of a wait cancels no task.
func childCommandRefusal(err error) relay.SwarmResponse {
	switch {
	case errors.Is(err, errChildNodePolicyMismatch):
		return relay.SwarmResponse{Text: "Stored child join policy differs from the parent node definition. History and child evidence are retained; inspect and explicitly reconcile before advancing the parent.", Outcome: "refused"}
	case errors.Is(err, store.ErrSwarmChildDenied), errors.Is(err, app.ErrNotAllowed), errors.Is(err, store.ErrNotFound), errors.Is(err, app.ErrNotFound):
		return relay.SwarmResponse{Text: "Child task or group is not available to this current actor and delegation.", Outcome: "refused"}
	case errors.Is(err, store.ErrStepReportInvalid):
		return relay.SwarmResponse{Text: fmt.Sprintf("Invalid child report: at most %d bytes of UTF-8 text without control characters other than newline and tab; no outcome was recorded.", store.MaxStepReportBytes), Outcome: "refused"}
	case errors.Is(err, store.ErrSwarmChildConflict), errors.Is(err, store.ErrCoordinationConflict), errors.Is(err, store.ErrPipelineVisitConflict), errors.Is(err, app.ErrConflict), stepChanged(err):
		return relay.SwarmResponse{Text: "Child request, policy, role, identity or state conflicts. Running and uncertain obligations cannot be cancelled or bypassed.", Outcome: "refused"}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return relay.SwarmResponse{Text: "Child command wait stopped; a mutation or parent join may already have been accepted. Durable intents and possible effects are retained, not cancelled or resent.", Outcome: "unconfirmed"}
	default:
		return childMutationUnconfirmed()
	}
}
