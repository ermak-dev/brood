package command_test

// BUG: A command refusal reached the client only as the raw error text in
// detail, indistinguishable from an internal error, so bro hid both (#276).
// SPEC: A deliberate refusal holds a public reason and an error class only.
// Its error text is the detail the server has always sent; PublicReason gives
// its reason when it is at most MaxReasonBytes of UTF-8 without control
// characters, and nothing for any other error, so the text of an internal
// error never becomes a reason, even next to a refusal.
// IMPLEMENTATION: command.Refusal, Refuse, Refusef and PublicReason.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

var errClass = errors.New("conflict")

func TestRefusalKeepsDetailAndClass(t *testing.T) {
	for name, tc := range map[string]struct {
		err           error
		class         error
		detail, cause string
	}{
		// Refusef заменяет fmt.Errorf("%w: ...", class): detail тот же.
		"labeled": {command.Refusef(errClass, "update refused: actor %q is stopped", "worker"), errClass, `conflict: update refused: actor "worker" is stopped`, `update refused: actor "worker" is stopped`},
		// Refuse — отказ класса, чей текст сам по себе причина (#152).
		"bare":     {command.Refuse(errClass, `actor command not authorized: "w" is another master`), errClass, `actor command not authorized: "w" is another master`, `actor command not authorized: "w" is another master`},
		"no class": {command.Refuse(nil, "restart is unsupported"), nil, "restart is unsupported", "restart is unsupported"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.detail {
				t.Errorf("detail %q, want %q", got, tc.detail)
			}
			if tc.class != nil && !errors.Is(tc.err, tc.class) {
				t.Errorf("refusal is not of its class")
			}
			if tc.class == nil && errors.Unwrap(tc.err) != nil {
				t.Errorf("classless refusal wraps %v", errors.Unwrap(tc.err))
			}
			var refusal *command.Refusal
			if !errors.As(tc.err, &refusal) || refusal.Reason() != tc.cause {
				t.Errorf("refusal reason %+v, want %q", refusal, tc.cause)
			}
			if got := command.PublicReason(tc.err); got != tc.cause {
				t.Errorf("public reason %q, want %q", got, tc.cause)
			}
		})
	}
}

func TestPublicReasonNeverCarriesInternalText(t *testing.T) {
	internal := errors.New("sqlite: no such table: synthetic-secret")
	for name, err := range map[string]error{
		"internal":            internal,
		"classed internal":    fmt.Errorf("%w: %w", errClass, internal),
		"wrapped internal":    fmt.Errorf("managed lifecycle refused: %w", internal),
		"nil":                 nil,
		"refusal-like string": errors.New("update refused: synthetic-secret"),
	} {
		if got := command.PublicReason(err); got != "" {
			t.Errorf("%s: public reason %q", name, got)
		}
	}
	// Отказ рядом с внутренней ошибкой: detail может нести её, как прежде,
	// причина — только текст отказа.
	for name, err := range map[string]error{
		"refusal then cause":  fmt.Errorf("%w: %v", command.Refuse(nil, "invalid coordination arguments"), internal),
		"cause wraps refusal": fmt.Errorf("transaction: %w", command.Refuse(nil, "invalid coordination arguments")),
		"joined":              errors.Join(internal, command.Refuse(nil, "invalid coordination arguments")),
	} {
		if got := command.PublicReason(err); got != "invalid coordination arguments" {
			t.Errorf("%s: public reason %q", name, got)
		}
	}
	// Refusal не хранит причину отказа: Unwrap отдаёт только класс.
	if errors.Is(command.Refusef(errClass, "managed lifecycle refused"), internal) {
		t.Error("refusal wraps an internal error")
	}
}

func TestPublicReasonBounds(t *testing.T) {
	longest := strings.Repeat("r", command.MaxReasonBytes)
	if got := command.PublicReason(command.Refuse(nil, longest)); got != longest {
		t.Errorf("reason of %d bytes dropped", len(longest))
	}
	for name, reason := range map[string]string{
		"empty":       "",
		"oversize":    longest + "r",
		"newline":     "refused\nforged line",
		"tab":         "refused\tcolumn",
		"escape":      "refused \x1b[31mred",
		"delete":      "refused \x7f",
		"c1":          "refused \u0085",
		"invalid utf": "refused \xff",
	} {
		err := command.Refuse(errClass, reason)
		if got := command.PublicReason(err); got != "" {
			t.Errorf("%s: public reason %q", name, got)
		}
		// Неподходящая причина не меняет detail.
		if err.Error() != reason {
			t.Errorf("%s: detail %q", name, err.Error())
		}
	}
}
