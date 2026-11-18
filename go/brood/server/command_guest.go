package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"brood.dev/go/brood/app"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// stepReportRefusal — фиксированная причина отказа неверному отчёту шага
// (#233): гость узнаёт, что поправить, а исход не записан.
var stepReportRefusal = fmt.Sprintf("invalid pipeline report: at most %d bytes of UTF-8 text without control characters other than newline and tab; the outcome was not recorded", store.MaxStepReportBytes)

func commandSwarmResponse(result command.Result) relay.SwarmResponse {
	ok := true
	switch result.Outcome {
	case "unconfirmed", "unknown", "failed", "refused", "unsupported":
		ok = false
	}
	if result.Text == "" {
		result.Text = "(empty)"
	}
	return relay.SwarmResponse{OK: ok, Text: result.Text, Data: result.Data, Outcome: result.Outcome}
}

func (m *managedProvider) handleActorCommand(ctx context.Context, from store.ManagedActor, inv command.Invocation) relay.SwarmResponse {
	switch inv.Name {
	case "pipeline_visit_complete":
		validated, err := command.ParseTool(inv.Name, inv.Arguments)
		if err != nil {
			return relay.SwarmResponse{Text: "invalid pipeline completion", Outcome: "refused"}
		}
		var args struct {
			RunID     string `json:"run_id"`
			Sequence  int64  `json:"sequence"`
			RequestID string `json:"request_id"`
			Outcome   string `json:"outcome"`
			Report    string `json:"report"`
		}
		if err := json.Unmarshal(validated.Arguments, &args); err != nil {
			return relay.SwarmResponse{Text: "invalid pipeline completion", Outcome: "refused"}
		}
		nonce, err := newPipelineCompletionNonce()
		if err != nil {
			return relay.SwarmResponse{Text: "completion acceptance receipt unavailable", Outcome: "failed"}
		}
		var acceptance json.RawMessage
		err = m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
			if args.RunID != current.SwarmID {
				return store.ErrPipelineVisitConflict
			}
			needsResult, err := m.server.store.PipelineCompletionUsesPolicy(ctx, current.SwarmID, args.Sequence)
			if err != nil {
				return err
			}
			if needsResult {
				step, err := m.server.store.GetSwarmStep(ctx, current.SwarmID, args.Sequence)
				if err != nil {
					return err
				}
				if step.State != "completed" {
					in := store.ActorCompletion{SwarmID: current.SwarmID, Sequence: args.Sequence, RequestID: args.RequestID, ActorUID: current.UID, Incarnation: current.Incarnation, Outcome: args.Outcome, Report: args.Report}
					receipt, err := m.server.store.AcceptPipelineCompletion(ctx, in, nonce, func(ctx context.Context) bool { return m.currentActorReceiptAuthority(ctx, current) })
					if err != nil {
						return err
					}
					acceptance, err = relay.WithCompletionReceipt(nil, completionReceiptMetadata(receipt))
					return err
				}
			}
			return m.server.store.CompletePipelineActorVisit(ctx, current.SwarmID, args.RunID, args.Sequence, args.RequestID, current.UID, current.Incarnation, args.Outcome, args.Report)
		})
		if err != nil {
			switch {
			case errors.Is(err, store.ErrStepReportInvalid):
				return relay.SwarmResponse{Text: stepReportRefusal, Outcome: "refused"}
			case errors.Is(err, app.ErrNotAllowed):
				return relay.SwarmResponse{Text: "pipeline completion not authorized", Outcome: "refused"}
			case errors.Is(err, store.ErrPipelineVisitInterrupted):
				return relay.SwarmResponse{Text: "Owner intervention interrupted this step; explicit reconciliation is required before completion.", Outcome: "refused"}
			case errors.Is(err, store.ErrSwarmChildConflict):
				return relay.SwarmResponse{Text: "child obligations gate this parent; use child_join to compute and complete its result", Outcome: "refused"}
			case errors.Is(err, app.ErrConflict), errors.Is(err, store.ErrPipelineVisitConflict):
				return relay.SwarmResponse{Text: "pipeline completion conflict", Outcome: "refused"}
			default:
				return relay.SwarmResponse{Text: "pipeline completion unavailable", Outcome: "failed"}
			}
		}
		if acceptance != nil {
			return relay.SwarmResponse{OK: true, Text: "pipeline completion accepted; result evidence and policy are pending", Data: acceptance, Outcome: "accepted"}
		}
		// Исход мог ввести граф в следующий actor-узел (#212).
		m.server.steps.kick(from.SwarmID)
		return relay.SwarmResponse{OK: true, Text: "pipeline visit completed", Outcome: "completed"}
	case "child_create", "child_get", "child_join", "child_cancel", "child_complete":
		return m.childCommand(ctx, from, inv)
	case "child_spawn", "child_job":
		return m.ephemeralJobCommand(ctx, from, inv)
	case "ask_operator":
		return m.askOperator(ctx, from, inv)
	case "profile":
		data, _ := json.Marshal(map[string]any{"self": from.Name, "role": from.Role, "swarm_id": from.SwarmID, "provider": "proxmox", "integration_branch": nil})
		return relay.SwarmResponse{OK: true, Text: string(data)}
	case "dispatch", "landing":
		result, err := m.executeCoordinationCommand(ctx, from, inv)
		if err != nil {
			return relay.SwarmResponse{Text: err.Error(), Outcome: "refused"}
		}
		return commandSwarmResponse(result)
	case "models":
		result, err := m.server.adminModels.models(ctx)
		if _, _, authErr := m.authorizeCommandActor(ctx, from); authErr != nil {
			return relay.SwarmResponse{Text: "actor authorization changed"}
		}
		if err != nil {
			return relay.SwarmResponse{Text: err.Error()}
		}
		return commandSwarmResponse(result)
	case "diagnose":
		data, _ := json.Marshal(map[string]any{"mode": "guest", "self": from.Name, "generation": from.Incarnation, "swarm_id": from.SwarmID, "relay": "connected"})
		return relay.SwarmResponse{OK: true, Text: string(data), Data: data, Outcome: "completed"}
	case "tail", "slash", "session_history", "session_terminal", "stat", "restart", "wipe", "recycle":
		result, err := m.executeGuestSessionCommand(ctx, from, inv)
		if err != nil {
			return relay.SwarmResponse{Text: err.Error(), Outcome: result.Outcome}
		}
		return commandSwarmResponse(result)
	case "agents":
		return m.handleSwarmRequest(ctx, from.UID, from.Incarnation, relay.SwarmRequest{Tool: "agents"})
	case "add", "delete", "stop", "start", "reset", "update":
		result, err := m.executeLifecycleCommand(ctx, from, inv)
		if err != nil {
			// Неизвестный исход update (#133) доходит до гостя как unknown,
			// а не как отказ без исхода.
			return relay.SwarmResponse{Text: err.Error(), Outcome: result.Outcome}
		}
		return commandSwarmResponse(result)
	case "send":
		var args struct {
			To          string `json:"to"`
			Message     string `json:"message"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		_ = json.Unmarshal(inv.Arguments, &args)
		return m.handleSwarmRequest(ctx, from.UID, from.Incarnation, relay.SwarmRequest{Tool: "send", To: args.To, Message: args.Message, WaitSeconds: args.WaitSeconds})
	}
	return relay.SwarmResponse{Text: "command service is not available"}
}
