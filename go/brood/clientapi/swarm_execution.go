package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// MaxSwarmGoalBytes is the server's limit of a pass goal (go/CONTRACT.md).
// The server checks every rule of the goal; the client only bounds what it
// reads from a goal file.
const MaxSwarmGoalBytes = 4096

// SwarmStart is the body of a swarm start: the workflow of the swarm, the
// goal of the new pass and, optionally, the commit the first start pins.
type SwarmStart struct {
	Definition string `json:"definition"`
	Goal       string `json:"goal"`
	Commit     string `json:"commit,omitempty"`
}

// SwarmDefinition is the identity of the pinned workflow snapshot.
type SwarmDefinition struct {
	Name     string `json:"name"`
	Version  int64  `json:"version"`
	Revision string `json:"revision"`
}

// SwarmSource is the pinned source snapshot of the swarm.
type SwarmSource struct {
	Origin string `json:"origin"`
	Commit string `json:"commit"`
}

// SwarmStep is the latest step of the swarm; Detail is the reason of a
// refused or unconfirmed actor step.
type SwarmStep struct {
	Sequence int64  `json:"sequence"`
	NodeID   string `json:"nodeId"`
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
}

// SwarmExecution is the swarm execution after a start or stop
// (go/CONTRACT.md): Goal is the goal of the current pass.
type SwarmExecution struct {
	SwarmID    string           `json:"swarmId"`
	Execution  string           `json:"execution"`
	Definition *SwarmDefinition `json:"definition"`
	Source     *SwarmSource     `json:"source"`
	Goal       string           `json:"goal,omitempty"`
	Step       *SwarmStep       `json:"step"`
}

// StartSwarm starts a new pass of the swarm with its goal. Without a commit
// the first start pins the commit the server resolves from the project
// branch; a restart runs on the pinned source.
func (c *Client) StartSwarm(ctx context.Context, swarm string, start SwarmStart) (SwarmExecution, error) {
	if swarm == "" || start.Definition == "" || start.Goal == "" {
		return SwarmExecution{}, errors.New("swarm, definition and goal are required")
	}
	body, err := json.Marshal(start)
	if err != nil {
		return SwarmExecution{}, errors.New("could not encode swarm start")
	}
	return c.swarmExecutionCall(ctx, swarmExecutionPath(swarm, "start"), body)
}

// StopSwarm stops the running or waiting pass of the swarm.
func (c *Client) StopSwarm(ctx context.Context, swarm string) (SwarmExecution, error) {
	if swarm == "" {
		return SwarmExecution{}, errors.New("swarm is required")
	}
	return c.swarmExecutionCall(ctx, swarmExecutionPath(swarm, "stop"), nil)
}

func (c *Client) swarmExecutionCall(ctx context.Context, path string, body []byte) (SwarmExecution, error) {
	var execution SwarmExecution
	if err := c.call(ctx, http.MethodPost, path, body, &execution); err != nil {
		return SwarmExecution{}, err
	}
	return execution, nil
}

func swarmExecutionPath(swarm, action string) string {
	return "/api/swarms/" + url.PathEscape(swarm) + "/" + action
}

// isSwarmExecutionPath узнаёт маршруты запуска и остановки роя:
// /api/swarms/{id}/start и /api/swarms/{id}/stop.
func isSwarmExecutionPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 5 && parts[0] == "" && parts[1] == "api" && parts[2] == "swarms" && (parts[4] == "start" || parts[4] == "stop")
}

// swarmExecutionRefusals — фиксированные причины отказов запуска и остановки
// (go/CONTRACT.md). Причина хранилища несёт подробность — состояние,
// определение, роль, — поэтому её фиксированная часть стоит в начале, а за
// ней «: » и подробность.
var swarmExecutionRefusals = []string{
	"invalid swarm start: expected {definition, goal, commit?} with a definition name and a goal",
	"swarm management denied",
	"swarm not found",
	"outstanding child obligations require explicit reconciliation before automatic progression",
}

var swarmExecutionRefusalPrefixes = []string{
	"invalid swarm goal: ",
	"workflow definition is not in the project catalog: ",
	"swarm state conflict: ",
	"swarm graph is pinned: ",
	"workflow graph is not executable: ",
	"swarm source is invalid: ",
	"swarm source changed during start: ",
	"cannot resolve the project branch: ",
	`role "`,
}

// maxSwarmExecutionRefusal — предел показываемой причины.
const maxSwarmExecutionRefusal = 1024

// publicSwarmExecutionError — причина отказа запуска или остановки, если она
// публичная. Тело недоверенное: в ошибку попадает только одна строка без
// управляющих символов до maxSwarmExecutionRefusal байт, которая совпадает с
// фиксированной причиной или начинается с её фиксированной части.
func publicSwarmExecutionError(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4097))
	if err != nil || len(data) > 4096 {
		return ""
	}
	var response struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(data, &response) != nil {
		return ""
	}
	detail := response.Detail
	if len(detail) > maxSwarmExecutionRefusal || strings.IndexFunc(detail, unicode.IsControl) >= 0 {
		return ""
	}
	for _, reason := range swarmExecutionRefusals {
		if detail == reason {
			return detail
		}
	}
	for _, prefix := range swarmExecutionRefusalPrefixes {
		if strings.HasPrefix(detail, prefix) && len(detail) > len(prefix) {
			return detail
		}
	}
	return ""
}
