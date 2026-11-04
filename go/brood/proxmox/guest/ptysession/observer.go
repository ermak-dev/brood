//go:build linux

package ptysession

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"github.com/charmbracelet/x/vt"
)

const maxPending = 64 * 1024
const maxReplayTail = 512 * 1024
const maxReplay = (MaxMessage - 8192) * 3 / 4

type observer struct {
	terminal     *vt.Emulator
	boundary     *ansi.Parser
	pending      []byte
	modes        map[ansi.Mode]bool
	replayUnsafe bool
	pen          *vt.Emulator
	primary      string
	regions      [2]string
	topMargins   [2]int
	saved        [2]string
	modesUnsafe  bool
	checkpoint   []byte
	trailing     []byte
	tailUnsafe   bool
}

func newObserver(columns, rows int) *observer {
	e := vt.NewEmulator(columns, rows)
	// Ответы VT намеренно не соединены с PTY. Закрытая pipe немедленно
	// отвергает их, не блокируя parser на DSR/DA/resize queries.
	_ = e.InputPipe().(io.Closer).Close()
	o := &observer{terminal: e, boundary: ansi.NewParser(), modes: map[ansi.Mode]bool{}, pen: vt.NewEmulator(2, 2), checkpoint: []byte("\x1bc")}
	_ = o.pen.InputPipe().(io.Closer).Close()
	o.boundary.SetDataSize(maxPending)
	e.SetScrollbackSize(262144 / columns)
	e.SetCallbacks(vt.Callbacks{EnableMode: func(m ansi.Mode) { o.modes[m] = true }, DisableMode: func(m ansi.Mode) { o.modes[m] = false }})
	// Публичный VT API не экспортирует активное перо. Второй маленький VT
	// применяет SGR и save/restore сам: здесь нет собственного SGR interpreter.
	e.RegisterCsiHandler('m', func(p ansi.Params) bool {
		_, _ = o.pen.WriteString(csiParams(p, "", 'm'))
		return false
	})
	for _, prefix := range []byte{0, '?'} {
		for _, final := range []byte{'h', 'l'} {
			e.RegisterCsiHandler(ansi.Command(prefix, 0, final), func(p ansi.Params) bool {
				if len(o.modes) > 256 {
					o.modesUnsafe = true
					return true
				}
				if prefix == '?' {
					for _, param := range p {
						mode := param.Param(-1)
						if (mode == 1047 || mode == 1049) && final == 'h' && !e.IsAltScreen() {
							o.primary = o.visual()
						}
						if mode == 1048 && final == 'h' {
							o.saved[o.screenIndex()] = o.regions[o.screenIndex()] + o.cursor()
						}
					}
					_, _ = o.pen.WriteString(csiParams(p, "?", final))
				}
				return false
			})
		}
	}
	for _, command := range []byte{'7', '8'} {
		e.RegisterEscHandler(int(command), func() bool {
			if command == '7' {
				o.saved[o.screenIndex()] = o.regions[o.screenIndex()] + o.cursor()
			}
			_, _ = o.pen.Write([]byte{0x1b, command})
			return false
		})
	}
	e.RegisterCsiHandler('r', func(p ansi.Params) bool {
		top, _, _ := p.Param(0, 1)
		bottom, _, _ := p.Param(1, e.Height())
		if top < 1 {
			top = 1
		}
		if bottom < 1 {
			bottom = e.Height()
		}
		if top < bottom {
			o.regions[o.screenIndex()] = csiParams(p, "", 'r')
			o.topMargins[o.screenIndex()] = top - 1
		}
		return false
	})
	e.RegisterCsiHandler('s', func(p ansi.Params) bool {
		if !o.modes[ansi.ModeLeftRightMargin] {
			o.saved[o.screenIndex()] = o.regions[o.screenIndex()] + o.cursor()
			_, _ = o.pen.WriteString("\x1b7")
		}
		return false
	})
	e.RegisterCsiHandler('u', func(p ansi.Params) bool { _, _ = o.pen.WriteString("\x1b8"); return false })
	e.RegisterEscHandler('c', func() bool {
		o.modes = map[ansi.Mode]bool{}
		o.primary = ""
		o.regions = [2]string{}
		o.topMargins = [2]int{}
		o.saved = [2]string{}
		o.modesUnsafe = false
		_, _ = o.pen.WriteString("\x1bc")
		return false
	})
	return o
}

func (o *observer) resize(columns, rows int) {
	if o.terminal.IsAltScreen() && o.primary != "" {
		primary := newObserver(o.terminal.Width(), o.terminal.Height())
		primary.write([]byte(o.primary))
		primary.resize(columns, rows)
		o.primary = primary.visual()
		primary.close()
	}
	o.terminal.Resize(columns, rows)
	o.terminal.SetScrollbackSize(262144 / columns)
	o.regions = [2]string{}
	o.topMargins = [2]int{}
	o.saveCheckpoint()
}
func (o *observer) close() {
	_ = o.terminal.Close()
	_ = o.pen.Close()
}

func (o *observer) write(data []byte) {
	// VT не экспортирует pending-wrap. Снимок до правой границы плюс
	// ограниченный сырой суффикс восстанавливают его без догадок о private state.
	if o.terminal.CursorPosition().X != o.terminal.Width()-1 {
		o.saveCheckpoint()
	}
	if len(o.trailing)+len(data) <= maxReplayTail {
		o.trailing = append(o.trailing, data...)
	} else {
		o.tailUnsafe = true
	}
	for _, b := range data {
		if o.boundary.State() != parser.GroundState || b == 0x1b || b >= 0x80 {
			if len(o.pending) < maxPending {
				o.pending = append(o.pending, b)
			} else {
				o.replayUnsafe = true
			}
		}
		o.boundary.Advance(b)
		if o.boundary.State() == parser.GroundState {
			o.pending = o.pending[:0]
			o.replayUnsafe = false
		}
	}
	_, _ = o.terminal.Write(data)
}

func (o *observer) saveCheckpoint() {
	if snapshot, err := o.snapshot(); err == nil {
		o.checkpoint = snapshot[:len(snapshot)-len(o.pending)]
		o.trailing = append(o.trailing[:0], o.pending...)
		o.tailUnsafe = false
	}
}
func (o *observer) replay() ([]byte, error) {
	if o.replayUnsafe || o.modesUnsafe {
		return o.snapshot()
	}
	if o.terminal.CursorPosition().X == o.terminal.Width()-1 {
		if o.tailUnsafe || len(o.checkpoint)+len(o.trailing) > maxReplay {
			return nil, fmt.Errorf("terminal pending-wrap tail exceeds replay limit")
		}
		replay := make([]byte, 0, len(o.checkpoint)+len(o.trailing))
		replay = append(replay, o.checkpoint...)
		return append(replay, replayTail(o.trailing)...), nil
	}
	return o.snapshot()
}

// Суффикс восстанавливает только состояние: повторные terminal queries
// заставили бы новый browser послать приложению второй ответ. Разбирает
// последовательности библиотечный ANSI parser; live stream не меняется.
func replayTail(raw []byte) []byte {
	p := ansi.NewParser()
	p.SetDataSize(maxReplayTail)
	query := false
	p.SetHandler(ansi.Handler{
		HandleCsi: func(cmd ansi.Cmd, _ ansi.Params) {
			query = cmd.Final() == 'c' || cmd.Final() == 'n' || cmd.Final() == 't' || (cmd.Final() == 'p' && cmd.Intermediate() == '$')
		},
		HandleDcs: func(cmd ansi.Cmd, _ ansi.Params, _ []byte) {
			query = cmd.Final() == 'q' && (cmd.Intermediate() == '$' || cmd.Intermediate() == '+')
		},
		HandleOsc: func(cmd int, data []byte) {
			if cmd == 4 || cmd == 10 || cmd == 11 || cmd == 12 || cmd == 52 {
				query = strings.HasSuffix(string(data), ";?")
			}
		},
	})
	filtered := make([]byte, 0, len(raw))
	start := 0
	for i, b := range raw {
		p.Advance(b)
		if p.State() == parser.GroundState {
			if !query {
				filtered = append(filtered, raw[start:i+1]...)
			}
			start = i + 1
			query = false
		}
	}
	// Незавершённый prefix принадлежит следующему настоящему live frame.
	return append(filtered, raw[start:]...)
}

func (o *observer) snapshot() ([]byte, error) {
	if o.replayUnsafe {
		return nil, fmt.Errorf("incomplete terminal sequence exceeds replay limit")
	}
	if o.modesUnsafe {
		return nil, fmt.Errorf("terminal mode count exceeds replay limit")
	}
	var b strings.Builder
	b.WriteString("\x1bc")
	if o.terminal.IsAltScreen() {
		b.WriteString(o.primary)
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString(o.visual())
	keys := make([]ansi.Mode, 0, len(o.modes))
	for m := range o.modes {
		keys = append(keys, m)
	}
	sort.Slice(keys, func(i, j int) bool {
		return fmt.Sprintf("%T:%d", keys[i], keys[i].Mode()) < fmt.Sprintf("%T:%d", keys[j], keys[j].Mode())
	})
	for _, m := range keys {
		if m == ansi.ModeOrigin || m == ansi.ModeAltScreen || m == ansi.ModeAltScreenSaveCursor || m == ansi.ModeSaveCursor {
			continue
		}
		if o.modes[m] {
			b.WriteString(ansi.SetMode(m))
		} else {
			b.WriteString(ansi.ResetMode(m))
		}
	}
	b.Write(o.pending)
	if b.Len() > maxReplay {
		return nil, fmt.Errorf("terminal snapshot exceeds replay limit")
	}
	return []byte(b.String()), nil
}

func csiParams(params ansi.Params, prefix string, final byte) string {
	var b strings.Builder
	b.WriteString("\x1b[" + prefix)
	for i, p := range params {
		if i > 0 {
			if params[i-1].HasMore() {
				b.WriteByte(':')
			} else {
				b.WriteByte(';')
			}
		}
		if n := p.Param(-1); n >= 0 {
			fmt.Fprint(&b, n)
		}
	}
	b.WriteByte(final)
	return b.String()
}

func (o *observer) visual() string {
	var b strings.Builder
	b.WriteString("\x1b[?6l\x1b[r\x1b[H\x1b[2J")
	b.WriteString(strings.ReplaceAll(o.terminal.Render(), "\n", "\r\n"))
	index := o.screenIndex()
	if o.saved[index] != "" {
		b.WriteString(o.saved[index])
		b.WriteString("\x1b7")
	}
	b.WriteString("\x1b[r")
	b.WriteString(o.regions[index])
	b.WriteString(o.cursor())
	return b.String()
}
func (o *observer) screenIndex() int {
	if o.terminal.IsAltScreen() {
		return 1
	}
	return 0
}
func (o *observer) cursor() string {
	var b strings.Builder
	p := o.terminal.CursorPosition()
	// DECOM homes real terminals; restore it before the origin-relative CUP.
	if o.modes[ansi.ModeOrigin] {
		b.WriteString("\x1b[?6h")
		p.Y -= o.topMargins[o.screenIndex()]
	} else {
		b.WriteString("\x1b[?6l")
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", p.Y+1, p.X+1)
	if o.terminal.CursorHidden() {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	style, blink := o.terminal.CursorStyle()
	n := int(style)*2 + 2
	if blink {
		n--
	}
	fmt.Fprintf(&b, "\x1b[%d q", n)
	_, _ = o.pen.WriteString("\x1b[H ")
	cell := o.pen.CellAt(0, 0)
	b.WriteString("\x1b[0m")
	b.WriteString(cell.Style.String())
	return b.String()
}
func (o *observer) tail(lines int) string {
	e := o.terminal
	var all []string
	for y := 0; y < e.ScrollbackLen(); y++ {
		var b strings.Builder
		for x := 0; x < e.Width(); x++ {
			c := e.ScrollbackCellAt(x, y)
			if c == nil {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Content)
			}
		}
		all = append(all, strings.TrimRight(b.String(), " "))
	}
	all = append(all, strings.Split(e.String(), "\n")...)
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
