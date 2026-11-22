// Package command defines the shared, transport-independent command catalog.
// Metadata is descriptive, not an authorization boundary. Executors must enforce
// authentication, scope, permissions, and destructive-operation preconditions.
package command

import (
	"context"
	"encoding/json"
	"strings"
)

// GuestToolList is an internal broker request, not a callable catalog tool.
const GuestToolList = "_guest_tool_list"

// Invocation is a validated tool name and canonical JSON argument object.
type Invocation struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Result is a transport-independent command response.
type Result struct {
	Text    string          `json:"text"`
	Data    json.RawMessage `json:"data,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
}

// Executor executes commands within an adapter's authenticated scope.
type Executor interface {
	Execute(context.Context, Invocation) (Result, error)
}

// Arg describes one scalar argument. Nil Default means the optional argument
// stays absent. Min and Max are inclusive numeric bounds. Required strings must
// not be empty. CLI flags use the argument name with underscores changed to dashes.
type Arg struct {
	Name        string
	Type        string
	Description string
	Required    bool
	Positional  bool
	Default     any
	Min         *float64
	Max         *float64
	Enum        []string
	MaxLength   int
}

// Spec is the single declaration used by CLI and tool adapters.
type Spec struct {
	Path        []string
	ToolName    string
	Description string
	Args        []Arg
	ReadOnly    bool
	Destructive bool
	Guest       bool
}

// Schema builds an independent MCP input schema from the argument declaration.
func (s Spec) Schema() map[string]any {
	properties := map[string]any{}
	required := []string{}
	for _, a := range s.Args {
		p := map[string]any{"type": a.Type}
		if a.Description != "" {
			p["description"] = a.Description
		}
		if a.Default != nil {
			p["default"] = a.Default
		}
		if a.Min != nil {
			p["minimum"] = *a.Min
		}
		if a.Max != nil {
			p["maximum"] = *a.Max
		}
		if a.Type == "string" {
			if a.Required {
				p["minLength"] = 1
			}
			if a.MaxLength > 0 {
				p["maxLength"] = a.MaxLength
			}
		}
		if len(a.Enum) > 0 {
			p["enum"] = append([]string(nil), a.Enum...)
		}
		properties[a.Name] = p
		if a.Required {
			required = append(required, a.Name)
		}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

var catalog = []Spec{
	agentsSpec, sendSpec, tailSpec, slashSpec,
	addSpec, rmSpec, startSpec, stopSpec, restartSpec, resetSpec,
	poolsSpec, poolDrainSpec, poolEnableSpec,
	diagnoseSpec, serverDiagnoseSpec,
}

// Specs returns independent catalog metadata in stable declaration order.
func Specs() []Spec {
	out := make([]Spec, len(catalog))
	for i, s := range catalog {
		out[i] = cloneSpec(s)
	}
	return out
}

// Lookup finds an exact tool name; CLI paths are parsed by ParseCLI instead.
func Lookup(name string) (Spec, bool) {
	for _, s := range catalog {
		if s.ToolName == name {
			return cloneSpec(s), true
		}
	}
	return Spec{}, false
}

func cloneSpec(s Spec) Spec {
	s.Path = append([]string(nil), s.Path...)
	s.Args = append([]Arg(nil), s.Args...)
	for i := range s.Args {
		a := &s.Args[i]
		a.Enum = append([]string(nil), a.Enum...)
		if a.Min != nil {
			v := *a.Min
			a.Min = &v
		}
		if a.Max != nil {
			v := *a.Max
			a.Max = &v
		}
	}
	return s
}

func spec(name, description string, readOnly, destructive, guest bool, args ...Arg) Spec {
	return Spec{Path: strings.Split(name, "_"), ToolName: name, Description: description, Args: args, ReadOnly: readOnly, Destructive: destructive, Guest: guest}
}

func requiredText(name string) Arg {
	return Arg{Name: name, Type: "string", Required: true, Positional: true, MaxLength: 262144}
}
func optionalText(name, def string) Arg {
	return Arg{Name: name, Type: "string", Default: def, MaxLength: 262144}
}
func integer(name string, def any, min, max float64) Arg {
	return Arg{Name: name, Type: "integer", Default: def, Min: &min, Max: &max}
}
func confirmArg() Arg { return Arg{Name: "confirm", Type: "boolean", Default: false} }
