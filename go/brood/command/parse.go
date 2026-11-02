package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxArgumentsBytes bounds both tool JSON and CLI argument input.
const MaxArgumentsBytes = 1 << 20

// ParseTool validates one JSON object, rejects duplicate/unknown keys, and
// returns sorted-key JSON with defaults materialized and numbers normalized.
// Empty input and null are not objects; callers with no arguments must use {}.
func ParseTool(name string, arguments json.RawMessage) (Invocation, error) {
	s, ok := Lookup(name)
	if !ok {
		return Invocation{}, fmt.Errorf("unknown command %q", name)
	}
	if len(arguments) > MaxArgumentsBytes {
		return Invocation{}, fmt.Errorf("arguments too large: maximum %d bytes", MaxArgumentsBytes)
	}
	if !utf8.Valid(arguments) {
		return Invocation{}, fmt.Errorf("arguments must be UTF-8 JSON")
	}
	d := json.NewDecoder(bytes.NewReader(arguments))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return Invocation{}, fmt.Errorf("arguments must be a JSON object")
	}
	values := map[string]any{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return Invocation{}, fmt.Errorf("invalid JSON: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return Invocation{}, fmt.Errorf("invalid JSON object key")
		}
		if _, exists := values[key]; exists {
			return Invocation{}, fmt.Errorf("duplicate argument %q", key)
		}
		a, ok := findArg(s, key)
		if !ok {
			return Invocation{}, fmt.Errorf("unknown argument %q for %s", key, name)
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return Invocation{}, fmt.Errorf("invalid JSON for %s: %w", key, err)
		}
		value, err := validate(a, raw)
		if err != nil {
			return Invocation{}, err
		}
		values[key] = value
	}
	if _, err := d.Token(); err != nil {
		return Invocation{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return Invocation{}, fmt.Errorf("arguments must contain exactly one JSON object")
	}
	for _, a := range s.Args {
		if _, exists := values[a.Name]; exists {
			continue
		}
		if a.Required {
			return Invocation{}, fmt.Errorf("required argument %q is missing", a.Name)
		}
		if a.Default != nil {
			values[a.Name] = a.Default
		}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return Invocation{}, fmt.Errorf("encode arguments: %w", err)
	}
	if len(raw) > MaxArgumentsBytes {
		return Invocation{}, fmt.Errorf("canonical arguments too large: maximum %d bytes", MaxArgumentsBytes)
	}
	return Invocation{Name: s.ToolName, Arguments: raw}, nil
}

func findArg(s Spec, name string) (Arg, bool) {
	for _, a := range s.Args {
		if a.Name == name {
			return a, true
		}
	}
	return Arg{}, false
}

func validate(a Arg, raw json.RawMessage) (any, error) {
	invalid := func(reason string) (any, error) { return nil, fmt.Errorf("argument %q %s", a.Name, reason) }
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalid("must not be null")
	}
	switch a.Type {
	case "string":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return invalid("must be a string")
		}
		if a.Required && v == "" {
			return invalid("must not be empty")
		}
		if a.MaxLength > 0 && utf8.RuneCountInString(v) > a.MaxLength {
			return invalid("is too large")
		}
		if len(a.Enum) > 0 {
			found := false
			for _, e := range a.Enum {
				if v == e {
					found = true
					break
				}
			}
			if !found {
				return invalid("must be one of: " + strings.Join(a.Enum, ", "))
			}
		}
		return v, nil
	case "boolean":
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return invalid("must be a boolean")
		}
		return v, nil
	case "integer", "number":
		// Decode into float64, not json.Number: quoted numbers are not JSON numbers.
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return invalid("must be a finite number")
		}
		// Bound exact arithmetic before parsing exponents, including underflow.
		text := string(bytes.TrimSpace(raw))
		if len(text) > 128 {
			return invalid("numeric literal is too large")
		}
		if i := strings.IndexAny(text, "eE"); i >= 0 {
			exponent, err := strconv.Atoi(text[i+1:])
			if err != nil || exponent < -1024 || exponent > 1024 {
				return invalid("numeric exponent is out of range")
			}
		}
		exact, ok := new(big.Rat).SetString(text)
		if !ok {
			return invalid("must be a number")
		}
		if a.Type == "integer" && !exact.IsInt() {
			return invalid("must be an integer")
		}
		if a.Min != nil && exact.Cmp(new(big.Rat).SetFloat64(*a.Min)) < 0 {
			return invalid(fmt.Sprintf("must be at least %g", *a.Min))
		}
		if a.Max != nil && exact.Cmp(new(big.Rat).SetFloat64(*a.Max)) > 0 {
			return invalid(fmt.Sprintf("must be at most %g", *a.Max))
		}
		if v == 0 {
			v = 0
		}
		return v, nil
	}
	return invalid("has an unsupported type")
}

// ParseCLI parses a catalog path, positional arguments, and long flags.
// Flags may be interspersed, accept --name=value or --name value, and map
// underscores to dashes. Boolean flags without a value mean true; use
// --confirm=false for false. -- ends flags for positional text starting with -.
// Authentication, project, and server flags belong to the caller, not this parser.
func ParseCLI(argv []string) (Invocation, error) {
	size := 0
	for _, v := range argv {
		if !utf8.ValidString(v) {
			return Invocation{}, fmt.Errorf("arguments must be UTF-8")
		}
		if len(v) > MaxArgumentsBytes-size {
			return Invocation{}, fmt.Errorf("arguments too large")
		}
		size += len(v)
	}
	var selected Spec
	used := 0
	for _, s := range catalog {
		if len(argv) < len(s.Path) {
			continue
		}
		match := true
		for i, p := range s.Path {
			if argv[i] != p {
				match = false
				break
			}
		}
		if match && len(s.Path) > used {
			selected = s
			used = len(s.Path)
		}
	}
	if used == 0 {
		return Invocation{}, fmt.Errorf("unknown command path")
	}
	values := map[string]any{}
	positionals := []Arg{}
	for _, a := range selected.Args {
		if a.Positional {
			positionals = append(positionals, a)
		}
	}
	pos := 0
	flags := true
	for i := used; i < len(argv); i++ {
		token := argv[i]
		if flags && token == "--" {
			flags = false
			continue
		}
		var a Arg
		var value any
		if flags && strings.HasPrefix(token, "--") {
			key, text, hasValue := strings.Cut(strings.TrimPrefix(token, "--"), "=")
			var ok bool
			a, ok = findArg(selected, strings.ReplaceAll(key, "-", "_"))
			if !ok {
				return Invocation{}, fmt.Errorf("unknown flag %q", key)
			}
			if !hasValue && a.Type == "boolean" {
				text = "true"
			} else if !hasValue {
				i++
				if i >= len(argv) || strings.HasPrefix(argv[i], "--") {
					return Invocation{}, fmt.Errorf("flag %q requires a value", key)
				}
				text = argv[i]
			}
			var err error
			value, err = cliValue(a, text)
			if err != nil {
				return Invocation{}, err
			}
		} else {
			if flags && strings.HasPrefix(token, "-") {
				return Invocation{}, fmt.Errorf("unknown flag %q; use -- before positional text", token)
			}
			if pos >= len(positionals) {
				return Invocation{}, fmt.Errorf("unexpected positional argument %q", token)
			}
			a = positionals[pos]
			pos++
			value = token
		}
		if _, exists := values[a.Name]; exists {
			return Invocation{}, fmt.Errorf("duplicate argument %q", a.Name)
		}
		values[a.Name] = value
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return Invocation{}, fmt.Errorf("encode arguments: %w", err)
	}
	return ParseTool(selected.ToolName, raw)
}

func cliValue(a Arg, text string) (any, error) {
	switch a.Type {
	case "string":
		return text, nil
	case "boolean":
		if text == "true" {
			return true, nil
		}
		if text == "false" {
			return false, nil
		}
		return nil, fmt.Errorf("argument %q must be true or false", a.Name)
	case "integer", "number":
		return validate(a, json.RawMessage(text))
	}
	return nil, fmt.Errorf("unsupported argument %q", a.Name)
}
