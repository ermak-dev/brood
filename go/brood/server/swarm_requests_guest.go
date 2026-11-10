package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// inCommandActorTx проверяет личность актора гостевого пути дважды:
// atespace — до транзакции (внешний вызов не держит соединение базы), затем
// UID, инкарнацию и привязку — в транзакции записи, где выполняется fn.
// fn получает актуальную строку актора: личность берётся из relay, а не из
// аргументов команды.
func (m *managedProvider) inCommandActorTx(ctx context.Context, from store.ManagedActor, fn func(context.Context, store.ManagedActor) error) error {
	actor, checked, err := m.authorizeCommandActor(ctx, from)
	if err != nil {
		return err
	}
	return m.server.store.WithTx(ctx, func(ctx context.Context) error {
		current, swarm, err := m.authorizeCommandActorDurable(ctx, actor)
		if err != nil {
			return err
		}
		if swarm.AtespaceName != checked.AtespaceName || swarm.AtespaceUID != checked.AtespaceUID {
			return app.ErrConflict
		}
		return fn(ctx, current)
	})
}

// askOperator открывает вопрос актора оператору своего роя (#210). Ответа
// гостевой путь не даёт: отвечает только пользователь с правом управлять
// роем через API роя.
func (m *managedProvider) askOperator(ctx context.Context, from store.ManagedActor, inv command.Invocation) relay.SwarmResponse {
	validated, err := command.ParseTool(inv.Name, inv.Arguments)
	if err != nil {
		return relay.SwarmResponse{Text: "invalid operator request: check kind, options and text size", Outcome: "refused"}
	}
	var args struct {
		Question string `json:"question"`
		Kind     string `json:"kind"`
		Context  string `json:"context"`
		Options  string `json:"options"`
	}
	if err := json.Unmarshal(validated.Arguments, &args); err != nil {
		return relay.SwarmResponse{Text: "invalid operator request: check kind, options and text size", Outcome: "refused"}
	}
	var opened store.SwarmRequest
	err = m.inCommandActorTx(ctx, from, func(ctx context.Context, current store.ManagedActor) error {
		var openErr error
		opened, openErr = m.server.store.OpenActorRequest(ctx, store.ActorRequestInput{
			SwarmID: current.SwarmID, ActorUID: current.UID, ActorIncarnation: current.Incarnation,
			Kind: args.Kind, Question: args.Question, Context: args.Context, Options: requestOptionLabels(args.Options),
		})
		return openErr
	})
	if err != nil {
		return askOperatorRefusal(err)
	}
	data, _ := json.Marshal(map[string]string{"request_id": opened.ID, "state": opened.State, "kind": opened.Kind})
	return relay.SwarmResponse{OK: true, Text: "operator request " + opened.ID + " is open; the answer arrives as a message from the operator", Data: data, Outcome: "completed"}
}

// requestOptionLabels разбирает варианты выбора: по одному на строке,
// пустые строки пропускаются. У вопроса актора исход не выбирает ребро,
// поэтому исход варианта — его подпись.
func requestOptionLabels(text string) []store.RequestOption {
	var options []store.RequestOption
	for _, line := range strings.Split(text, "\n") {
		if label := strings.TrimSpace(line); label != "" {
			options = append(options, store.RequestOption{Label: label, Outcome: label})
		}
	}
	return options
}

// askOperatorRefusal — фиксированные причины отказа гостю.
func askOperatorRefusal(err error) relay.SwarmResponse {
	switch {
	case errors.Is(err, store.ErrSwarmRequestLimit):
		return relay.SwarmResponse{Text: "too many open operator requests; wait for answers", Outcome: "refused"}
	case errors.Is(err, store.ErrSwarmRequestInvalid):
		return relay.SwarmResponse{Text: "invalid operator request: check kind, options and text size", Outcome: "refused"}
	case errors.Is(err, app.ErrNotAllowed), errors.Is(err, store.ErrSwarmRequestDenied), errors.Is(err, store.ErrSwarmRequestStale):
		return relay.SwarmResponse{Text: "operator request not authorized", Outcome: "refused"}
	case errors.Is(err, app.ErrConflict):
		return relay.SwarmResponse{Text: "operator request conflict", Outcome: "refused"}
	default:
		return relay.SwarmResponse{Text: "operator request unavailable", Outcome: "failed"}
	}
}
