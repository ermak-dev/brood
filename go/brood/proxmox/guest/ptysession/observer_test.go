//go:build linux

package ptysession

// FEATURE: Reconnect с атомарным VT snapshot и целой границей parser.
// SPEC: Сохраняются cursor, modes, UTF-8 и escape prefixes; queries не блокируют.
// RESULT: IMPLEMENTED — RED регрессии cursor/SGR/alternate/margins/wrap исправлены.
// STATUS: DONE

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

func replayTerminal(t *testing.T, o *observer) *vt.Emulator {
	t.Helper()
	data, err := o.replay()
	if err != nil {
		t.Fatal(err)
	}
	other := newObserver(o.terminal.Width(), o.terminal.Height())
	t.Cleanup(func() { _ = other.terminal.Close() })
	other.write(data)
	return other.terminal
}
func TestReplayPartialBoundariesAndCursor(t *testing.T) {
	for _, parts := range [][2]string{{"before\xe2\x82", "\xacafter"}, {"before\x1b[3;", "7Hafter"}, {"before\x1b]2;title", "\x1b\\after"}} {
		o := newObserver(80, 24)
		o.write([]byte(parts[0]))
		other := replayTerminal(t, o)
		o.write([]byte(parts[1]))
		_, _ = other.Write([]byte(parts[1]))
		if other.String() != o.terminal.String() || other.CursorPosition() != o.terminal.CursorPosition() {
			t.Fatalf("split %q/%q changed replay: %q vs %q", parts[0], parts[1], other.String(), o.terminal.String())
		}
		_ = o.terminal.Close()
	}
}
func TestReplayCursorStyleAndCurrentPen(t *testing.T) {
	o := newObserver(80, 24)
	defer o.terminal.Close()
	o.write([]byte("\x1b[3 q\x1b[31mred\x1b[5;8H"))
	other := replayTerminal(t, o)
	wantStyle, wantBlink := o.terminal.CursorStyle()
	gotStyle, gotBlink := other.CursorStyle()
	if gotStyle != wantStyle || gotBlink != wantBlink {
		t.Fatalf("cursor style %v/%v != %v/%v", gotStyle, gotBlink, wantStyle, wantBlink)
	}
	o.write([]byte("x"))
	_, _ = other.Write([]byte("x"))
	a, b := o.terminal.CellAt(7, 4), other.CellAt(7, 4)
	if !a.Style.Equal(&b.Style) {
		t.Fatal("reconnect lost current pen")
	}
}
func TestReplayPrimaryScreenSurvivesAlternateScreen(t *testing.T) {
	o := newObserver(80, 24)
	defer o.terminal.Close()
	o.write([]byte("primary\x1b[?1049h\x1b[?25l\x1b[3;5Halternate"))
	other := replayTerminal(t, o)
	if other.String() != o.terminal.String() || !other.IsAltScreen() || !other.CursorHidden() {
		t.Fatal("alternate snapshot changed")
	}
	o.write([]byte("\x1b[?1049l"))
	_, _ = other.Write([]byte("\x1b[?1049l"))
	if other.String() != o.terminal.String() {
		t.Fatalf("primary screen lost: %q vs %q", other.String(), o.terminal.String())
	}
}
func TestReplayOriginAndSavedCursor(t *testing.T) {
	for _, sequence := range [][2]string{{"\x1b[2;20r\x1b[?6h\x1b[3;4Htext", "\x1b[1;1Hx"}, {"\x1b[4;5H\x1b[32m\x1b7\x1b[10;12H\x1b[31mtext", "\x1b8x"}, {"\x1b[4;5H\x1b[32m\x1b[?1048h\x1b[10;12H\x1b[31mtext", "\x1b[?1048lx"}} {
		o := newObserver(80, 24)
		o.write([]byte(sequence[0]))
		other := replayTerminal(t, o)
		if other.CursorPosition() != o.terminal.CursorPosition() {
			t.Fatalf("origin cursor changed: %v vs %v", other.CursorPosition(), o.terminal.CursorPosition())
		}
		o.write([]byte(sequence[1]))
		_, _ = other.Write([]byte(sequence[1]))
		if other.String() != o.terminal.String() || other.CursorPosition() != o.terminal.CursorPosition() {
			t.Fatalf("saved cursor changed: %q vs %q", other.String(), o.terminal.String())
		}
		_ = o.terminal.Close()
	}
}

func TestPendingWrapReplayDoesNotReissueQueries(t *testing.T) {
	o := newObserver(20, 5)
	defer o.close()
	o.write([]byte(strings.Repeat("a", 20) + "\x1b[6n\x1b[c\x1b[?2004$p\x1b]10;?\x1b\\"))
	replay, err := o.replay()
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"\x1b[6n", "\x1b[c", "\x1b[?2004$p", "\x1b]10;?"} {
		if bytes.Contains(replay, []byte(query)) {
			t.Fatalf("snapshot reissued query %q", query)
		}
	}
	other := replayTerminal(t, o)
	o.write([]byte("x"))
	_, _ = other.Write([]byte("x"))
	if other.String() != o.terminal.String() {
		t.Fatal("query removal lost pending wrap")
	}
}

func TestReplayPendingWrap(t *testing.T) {
	for _, suffix := range []string{"x", "\x1b[31mx", "\x1b[5;20Hx"} {
		o := newObserver(20, 5)
		o.write([]byte(strings.Repeat("a", 20)))
		other := replayTerminal(t, o)
		o.write([]byte(suffix))
		_, _ = other.Write([]byte(suffix))
		if other.String() != o.terminal.String() || other.CursorPosition() != o.terminal.CursorPosition() {
			t.Fatalf("pending wrap corrupted: %q vs %q", other.String(), o.terminal.String())
		}
		o.close()
	}
}

func TestReplayAlternateResizePreservesPrimary(t *testing.T) {
	o := newObserver(80, 24)
	defer o.terminal.Close()
	o.write([]byte(strings.Repeat("p", 60) + "\x1b[?1049halt"))
	o.resize(40, 10)
	other := replayTerminal(t, o)
	o.write([]byte("\x1b[?1049l"))
	_, _ = other.Write([]byte("\x1b[?1049l"))
	if other.String() != o.terminal.String() {
		t.Fatalf("alternate resize corrupted primary: %q vs %q", other.String(), o.terminal.String())
	}
}

func TestReplayAfterResizeResetsMargins(t *testing.T) {
	o := newObserver(80, 24)
	defer o.terminal.Close()
	o.write([]byte("\x1b[3;15r\x1b[?6h\x1b[4;5Hbefore"))
	o.resize(90, 30)
	other := replayTerminal(t, o)
	o.write([]byte("\x1b[1;1Hafter"))
	_, _ = other.Write([]byte("\x1b[1;1Hafter"))
	if other.String() != o.terminal.String() {
		t.Fatalf("resize restored stale margins: %q vs %q", other.String(), o.terminal.String())
	}
}

func TestOversizeReplayIsExplicitlyRefused(t *testing.T) {
	o := newObserver(20, 5)
	defer o.close()
	o.write([]byte("\x1b]8;;https://example.test/" + strings.Repeat("a", 500000) + "\x1b\\" + strings.Repeat("x", 100) + "\x1b[1;1H"))
	if replay, err := o.replay(); err == nil {
		t.Fatalf("oversized replay accepted: %d bytes", len(replay))
	}
}

func TestObserverModeCountIsBounded(t *testing.T) {
	for _, prefix := range []string{"", "?"} {
		o := newObserver(80, 24)
		for mode := 3000; mode < 3400; mode++ {
			o.write([]byte(fmt.Sprintf("\x1b[%s%dh", prefix, mode)))
		}
		if len(o.modes) > 288 {
			t.Fatalf("unbounded mode map: %d", len(o.modes))
		}
		if _, err := o.replay(); err == nil {
			t.Fatal("incomplete mode state was replayed")
		}
		o.close()
	}
}

func TestObserverSequenceAndHistoryBounds(t *testing.T) {
	o := newObserver(80, 5)
	defer o.terminal.Close()
	o.write([]byte("\x1b]2;" + strings.Repeat("a", maxPending+1)))
	if _, err := o.replay(); err == nil {
		t.Fatal("unsafe truncated prefix replayed")
	}
	o.write([]byte("\x07\x1b[6n\x1b[c\r\nlast"))
	if _, err := o.replay(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		o.write([]byte("\r\nline"))
	}
	o.write([]byte("\r\nfinal"))
	if got := o.tail(3); !strings.HasSuffix(got, "final") || strings.Count(got, "\n") != 2 || bytes.ContainsRune([]byte(got), '\x1b') {
		t.Fatalf("tail: %q", got)
	}
}
