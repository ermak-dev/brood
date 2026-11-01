//go:build linux

package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

const mcpVersion = "2025-03-26"

func mcpTool(name, description string, properties map[string]any, required []string, readOnly bool) map[string]any {
	return map[string]any{"name": name, "description": description,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
		"annotations": map[string]bool{"readOnlyHint": readOnly, "destructiveHint": false, "openWorldHint": false}}
}

func swarmTools() []map[string]any {
	return []map[string]any{
		mcpTool("agents", "List managed actors in this swarm. Names are addresses for send; running does not mean idle or a clean workspace.", map[string]any{}, []string{}, true),
		mcpTool("send", "Send a message to another managed actor in this swarm. Only a confirmed receipt proves delivery; unconfirmed means the outcome is unknown.", map[string]any{
			"to":           map[string]any{"type": "string", "minLength": 1, "maxLength": 63},
			"message":      map[string]any{"type": "string", "minLength": 1},
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 300},
		}, []string{"to", "message"}, false),
	}
}

// ServeMCP implements the tools-only MCP stdio lifecycle with bounded frames.
// The injected broker is the only authority; no tool can select an identity,
// endpoint, executable, credential or socket path.
func ServeMCP(ctx context.Context, input io.Reader, output io.Writer, call relay.SwarmCaller) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), relay.MaxMessageBytes)
	enc := json.NewEncoder(output)
	initialized, ready := false, false
	for scanner.Scan() {
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
					response["result"] = map[string]any{"protocolVersion": mcpVersion, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": false}}, "serverInfo": map[string]string{"name": "brood", "version": "1"}, "instructions": "Use agents and send for managed actors in this swarm. Built-in session messaging does not reach remote actors. Never interpret running as idle, or unconfirmed as delivered."}
				case "ping":
					response["result"] = map[string]any{}
				case "tools/list":
					if !ready {
						fail(-32002, "Server is not initialized")
						break
					}
					response["result"] = map[string]any{"tools": swarmTools()}
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
					if !strictMCPJSON(req.Params, &params) || (params.Name != "agents" && params.Name != "send") {
						fail(-32602, "Unknown tool or invalid arguments")
						break
					}
					if len(params.Arguments) == 0 {
						params.Arguments = json.RawMessage(`{}`)
					}
					var args struct {
						To          string `json:"to,omitempty"`
						Message     string `json:"message,omitempty"`
						WaitSeconds int    `json:"wait_seconds,omitempty"`
					}
					if !strictMCPJSON(params.Arguments, &args) {
						fail(-32602, "Invalid tool arguments")
						break
					}
					wire, _ := json.Marshal(relay.SwarmRequest{Tool: params.Name, To: args.To, Message: args.Message, WaitSeconds: args.WaitSeconds})
					request, valid := relay.DecodeSwarmRequest(wire)
					if !valid {
						fail(-32602, "Invalid tool arguments")
						break
					}
					callCtx, cancel := context.WithTimeout(ctx, time.Duration(args.WaitSeconds)*time.Second+15*time.Second)
					result := call(callCtx, request)
					cancel()
					response["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": result.Text}}, "isError": !result.OK}
				default:
					fail(-32601, "Method not found")
				}
			}
		}
		if err := enc.Encode(response); err != nil {
			return errors.New("MCP output unavailable")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if scanner.Err() != nil {
		return errors.New("MCP input exceeds frame limit or is unavailable")
	}
	return nil
}

func strictMCPJSON(data []byte, target any) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(target) == nil && d.Decode(new(any)) == io.EOF
}
