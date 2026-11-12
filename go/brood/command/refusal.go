package command

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxReasonBytes bounds the public reason of a command refusal. A longer
// reason is not sent (#276).
const MaxReasonBytes = 1024

// Refusal is a deliberate command refusal. It holds only a public reason,
// written for the caller of the command (an operator CLI or a model), and an
// error class that adapters map to a status. It never holds the error that
// led to it, so the text of an internal error cannot reach its reason; a
// caller may still wrap a refusal with such text for the detail it has
// always sent.
type Refusal struct {
	class  error
	reason string
	// labeled — текст ошибки начинается с класса, как у fmt.Errorf("%w: ..."),
	// который Refusef заменяет: detail и текст гостю остаются прежними.
	labeled bool
}

// Refuse returns a refusal of class (nil for none) whose error text is reason.
func Refuse(class error, reason string) error {
	return &Refusal{class: class, reason: reason}
}

// Refusef returns a refusal of class whose reason is fmt.Sprintf(format,
// args...) and whose error text is "<class>: <reason>", the text of the
// fmt.Errorf("%w: "+format, class, args...) it replaces. The arguments must be
// public values, never an error.
func Refusef(class error, format string, args ...any) error {
	return &Refusal{class: class, reason: fmt.Sprintf(format, args...), labeled: class != nil}
}

// Error returns the refusal as the detail of a command error.
func (r *Refusal) Error() string {
	if r.labeled {
		return r.class.Error() + ": " + r.reason
	}
	return r.reason
}

// Unwrap returns the error class, nothing else.
func (r *Refusal) Unwrap() error { return r.class }

// Reason returns the public reason as written, without the bounds of
// PublicReason.
func (r *Refusal) Reason() string { return r.reason }

// PublicReason returns the reason of the first refusal in err's chain, or ""
// when err carries no refusal or the reason is empty, longer than
// MaxReasonBytes, not UTF-8 or contains a control character.
func PublicReason(err error) string {
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		return ""
	}
	reason := refusal.reason
	if reason == "" || len(reason) > MaxReasonBytes || !utf8.ValidString(reason) || strings.IndexFunc(reason, unicode.IsControl) >= 0 {
		return ""
	}
	return reason
}
