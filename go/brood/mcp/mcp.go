// Package mcp exposes the shared command catalog over bounded JSON-RPC stdio.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"brood.dev/go/brood/command"
)

// ErrInvalidArguments lets an executor answer with a protocol-level argument
// refusal when its transport rejects the request.
var ErrInvalidArguments = errors.New("Invalid tool arguments")

const maxFrameBytes = 1 << 20
const version = "2025-03-26"
const instructions = "brood runs coding agents in containers of their own and joins them into a swarm with one human owner. Call agents to learn your name, your role and the other agents of the swarm. Use send to talk to another agent, or to the human owner with to=operator; the answer arrives later as a message, not as the result of send. Use tail to read what another agent printed last. A master may create agents with add and delete the agents it created with rm. An accepted operation is not a completed one: check the result with agents or tail, and when an outcome is reported as unconfirmed, look before you repeat the command."

// GuestSpecs returns catalog commands available through a guest adapter.
// The executor remains responsible for authorization.
func GuestSpecs() []command.Spec {
	var specs []command.Spec
	for _, spec := range command.Specs() {
		if spec.Guest {
			specs = append(specs, spec)
		}
	}
	return specs
}

// Serve implements the tools-only MCP lifecycle. Specs limits advertised and
// callable tools, never authorization. A nil executor supports discovery only.
func Serve(ctx context.Context, input io.Reader, output io.Writer, executor command.Executor, specs []command.Spec) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	encoder := json.NewEncoder(output)
	initialized, ready := false, false
	tools := make([]map[string]any, 0, len(specs))
	allowed := map[string]bool{}
	for _, spec := range specs {
		allowed[spec.ToolName] = true
		tools = append(tools, map[string]any{"name": spec.ToolName, "description": spec.Description, "inputSchema": spec.Schema(), "annotations": map[string]bool{"readOnlyHint": spec.ReadOnly, "destructiveHint": spec.Destructive, "openWorldHint": false}})
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		response := map[string]any{"jsonrpc": "2.0", "id": nil}
		fail := func(code int, message string) { response["error"] = map[string]any{"code": code, "message": message} }
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			fail(-32700, "Parse error")
		} else {
			if len(req.ID) > 0 {
				response["id"] = req.ID
			}
			if req.JSONRPC != "2.0" || req.Method == "" {
				fail(-32600, "Invalid request")
			} else if len(req.ID) == 0 {
				if req.Method == "notifications/initialized" && initialized {
					ready = true
				}
				continue
			} else {
				switch req.Method {
				case "initialize":
					if initialized {
						fail(-32600, "Already initialized")
						break
					}
					initialized = true
					response["result"] = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": false}}, "serverInfo": map[string]string{"name": "brood", "version": "1"}, "instructions": instructions}
				case "ping":
					response["result"] = map[string]any{}
				case "tools/list":
					if !ready {
						fail(-32002, "Server is not initialized")
						break
					}
					response["result"] = map[string]any{"tools": tools}
				case "tools/call":
					if !ready {
						fail(-32002, "Server is not initialized")
						break
					}
					var params struct {
						Name      string                     `json:"name"`
						Arguments json.RawMessage            `json:"arguments"`
						Meta      map[string]json.RawMessage `json:"_meta,omitempty"`
					}
					if !strictJSON(req.Params, &params) || !allowed[params.Name] {
						fail(-32602, "Unknown tool or invalid arguments")
						break
					}
					if len(params.Arguments) == 0 {
						params.Arguments = json.RawMessage(`{}`)
					}
					invocation, err := command.ParseTool(params.Name, params.Arguments)
					if err != nil {
						fail(-32602, err.Error())
						break
					}
					result := command.Result{}
					err = errors.New("command executor unavailable")
					if executor != nil {
						callCtx, cancel := context.WithTimeout(ctx, callTimeout(invocation.Arguments))
						result, err = executor.Execute(callCtx, invocation)
						cancel()
					}
					if errors.Is(err, ErrInvalidArguments) {
						fail(-32602, err.Error())
						break
					}
					failed := err != nil || result.Outcome == "unconfirmed"
					if err != nil {
						result.Text = err.Error()
					}
					response["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": result.Text}}, "isError": failed, "structuredContent": result}
				default:
					fail(-32601, "Method not found")
				}
			}
		}
		if encoder.Encode(response) != nil {
			return errors.New("MCP output unavailable")
		}
	}
	if scanner.Err() != nil {
		return errors.New("MCP input exceeds frame limit or is unavailable")
	}
	return ctx.Err()
}

func strictJSON(data []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

func callTimeout(args json.RawMessage) time.Duration {
	var timing struct {
		WaitSeconds int `json:"wait_seconds"`
	}
	_ = json.Unmarshal(args, &timing)
	if timing.WaitSeconds < 0 || timing.WaitSeconds > 300 {
		timing.WaitSeconds = 0
	}
	return command.CallTimeout(timing.WaitSeconds)
}
