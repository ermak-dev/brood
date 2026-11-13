package telegram

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MessageLimit is the Bot API limit of a message text, in characters.
	MessageLimit = 4096
	// CaptionLimit is the Bot API limit of a document caption, in characters.
	CaptionLimit = 1024
)

// TextLength returns the length of s in UTF-16 code units. A code point
// outside the Basic Multilingual Plane counts as two, so a text within a
// Bot API limit by TextLength is within it however Telegram counts
// characters. An invalid UTF-8 byte counts as one.
func TextLength(s string) int {
	n := 0
	for _, r := range s {
		n += units(r)
	}
	return n
}

func units(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

// SplitText cuts text into pieces of at most MessageLimit characters
// (TextLength) for consecutive messages. A piece ends after the last line
// break in its second half, else after the last space there, else at the
// limit, never inside a code point. Joined, the pieces give the text back,
// except that a piece of whitespace only, which Telegram refuses as an empty
// message, is dropped; blank text gives no pieces. Request material that
// must stay whole goes as a document instead (SendDocument).
func SplitText(text string) []string {
	var pieces []string
	for text != "" {
		cut := fit(text, MessageLimit)
		if cut < len(text) {
			cut = breakAt(text[:cut])
		}
		if strings.TrimSpace(text[:cut]) != "" {
			pieces = append(pieces, text[:cut])
		}
		text = text[cut:]
	}
	return pieces
}

// fit returns the byte length of the longest prefix of s within limit units.
func fit(s string, limit int) int {
	n := 0
	for i, r := range s {
		if n+units(r) > limit {
			return i
		}
		n += units(r)
	}
	return len(s)
}

// breakAt returns where a full piece ends: after the last line break in its
// second half, else after the last space there, else at its end.
func breakAt(window string) int {
	half := len(window) / 2
	if i := strings.LastIndexByte(window, '\n'); i >= half {
		return i + 1
	}
	if i := strings.LastIndexFunc(window, unicode.IsSpace); i >= half {
		_, size := utf8.DecodeRuneInString(window[i:])
		return i + size
	}
	return len(window)
}
