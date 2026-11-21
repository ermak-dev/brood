package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"brood.dev/go/brood/command"
)

const (
	// maxCommandRefusal — предел показываемой причины отказа команды (#276).
	maxCommandRefusal = 1024
	// maxCommandRefusalBody — предел читаемого тела отказа, как у других путей.
	maxCommandRefusalBody = 4096
)

// publicCommandRefusal — причина отказа команды из поля reason ответа
// (#276). Тело недоверенное, а detail сервер заполняет текстом любой ошибки,
// в том числе внутренней, поэтому detail и прочие поля не читаются. Причина
// попадает в ошибку, только если тело — UTF-8 до maxCommandRefusalBody байт,
// а она — непустая строка до maxCommandRefusal байт без управляющих
// символов. Сервер до #276 reason не отдаёт: ошибка остаётся прежней.
func publicCommandRefusal(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxCommandRefusalBody+1))
	if err != nil || len(data) > maxCommandRefusalBody || !utf8.Valid(data) {
		return ""
	}
	var response struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(data, &response) != nil {
		return ""
	}
	reason := response.Reason
	if reason == "" || len(reason) > maxCommandRefusal || strings.IndexFunc(reason, unicode.IsControl) >= 0 {
		return ""
	}
	return reason
}

// CommandError is a command the server answered with an error status. Reason
// is the public refusal reason, empty when the server gave none. Unconfirmed
// reports that the command may have taken effect anyway.
type CommandError struct {
	Status      int
	Reason      string
	Unconfirmed bool
	text        string
	cause       error
}

// Unwrap returns the transport error behind an unconfirmed outcome, if any.
func (e *CommandError) Unwrap() error { return e.cause }

func (e *CommandError) Error() string {
	text := e.text
	if e.Reason != "" {
		text += ": " + e.Reason
	}
	if e.Unconfirmed {
		text = "command outcome is unconfirmed; check before retrying: " + text
	}
	return text
}

// showsDetail узнаёт пути роёв, агентов и пулов: их detail пишет сервер для
// человека, и CLI показывает его как есть.
func showsDetail(path string) bool {
	for _, prefix := range []string{"/api/swarms", "/api/agents", "/api/proxmox-pools"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	rest, found := strings.CutPrefix(path, "/api/projects/")
	return found && strings.HasSuffix(rest, "/swarms")
}

// CommandExecutor binds catalog commands to an authenticated operator's scope.
// It preserves Client TLS, CSRF, redirect refusal, and credential handling.
type CommandExecutor struct {
	Client  *Client
	SwarmID string
}

// Execute posts canonical arguments to the server-owned command dispatcher.
func (e CommandExecutor) Execute(ctx context.Context, invocation command.Invocation) (command.Result, error) {
	if e.Client == nil {
		return command.Result{}, errors.New("operator client unavailable")
	}
	if e.SwarmID == "" && command.NeedsSwarm(invocation.Name) {
		return command.Result{}, errors.New("no swarm selected; choose one with: bro join NAME")
	}
	invocation, err := command.ParseTool(invocation.Name, invocation.Arguments)
	if err != nil {
		return command.Result{}, err
	}
	body, err := json.Marshal(struct {
		SwarmID   string          `json:"swarm_id"`
		Arguments json.RawMessage `json:"arguments"`
	}{e.SwarmID, invocation.Arguments})
	if err != nil {
		return command.Result{}, err
	}
	budget := 30 * time.Second
	if invocation.Name == "send" {
		var args struct {
			WaitSeconds int `json:"wait_seconds"`
		}
		if err := json.Unmarshal(invocation.Arguments, &args); err != nil {
			return command.Result{}, err
		}
		budget = max(budget, time.Duration(args.WaitSeconds)*time.Second+15*time.Second)
	}
	var result command.Result
	err = e.Client.callWithTimeout(ctx, http.MethodPost, "/api/commands/"+invocation.Name, body, &result, budget)
	return result, err
}
