//go:build linux

package ptysession

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxInput = 16 * 1024
const maxConnections = 32
const queueFrames = 32
const writeTimeout = 2 * time.Second

type child struct {
	cmd      *exec.Cmd
	master   *os.File
	observer *observer
	state    State
	proof    string
	done     chan struct{}
	readDone chan struct{}
	exitErr  error
}
type subscription struct {
	conn     net.Conn
	frames   chan Frame
	child    *child
	readOnly bool
}

// Host owns at most one child. All screen updates, subscriptions, mutations and
// writer ownership changes share one lock, including the replay/live handoff.
type Host struct {
	mu          sync.Mutex
	config      Config
	current     *child
	writer      *subscription
	subscribers map[*subscription]bool
	connections map[net.Conn]bool
	listener    net.Listener
	closing     bool
	closeOnce   sync.Once
	closed      chan struct{}
	closeErr    error
	finishGroup func(int) error
}

// New validates trusted configuration but does not start a child.
func New(config Config) (*Host, error) {
	return newHost(config, finishPinnedGroup)
}

func newHost(config Config, finishGroup func(int) error) (*Host, error) {
	if len(config.Command) == 0 || config.Command[0] == "" {
		return nil, errors.New("PTY command is required")
	}
	if config.Columns == 0 {
		config.Columns = 80
	}
	if config.Rows == 0 {
		config.Rows = 24
	}
	if !validSize(config.Columns, config.Rows) {
		return nil, errors.New("PTY dimensions must be 20..240 columns and 5..80 rows")
	}
	config.Command = append([]string(nil), config.Command...)
	// Даже nil означает пустую доверенную среду, а не os.Environ.
	config.Env = append([]string{}, config.Env...)
	return &Host{config: config, subscribers: map[*subscription]bool{}, connections: map[net.Conn]bool{}, closed: make(chan struct{}), finishGroup: finishGroup}, nil
}
func validSize(c, r int) bool { return c >= 20 && c <= 240 && r >= 5 && r <= 80 }
func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", errors.New("incomplete process stat")
	}
	return fields[19], nil
}
func openPTY(columns, rows int) (*os.File, *os.File, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	fail := func(e error) (*os.File, *os.File, error) { _ = master.Close(); return nil, nil, e }
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	if err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(columns), Row: uint16(rows)}); err != nil {
		_ = slave.Close()
		return fail(err)
	}
	return master, slave, nil
}
func (h *Host) startLocked() error {
	if h.current != nil && h.current.exitErr != nil {
		return h.current.exitErr
	}
	if h.current != nil && h.current.state.Running {
		return nil
	}
	c := h.config
	master, slave, err := openPTY(c.Columns, c.Rows)
	if err != nil {
		return err
	}
	defer slave.Close()
	cmd := exec.Command(c.Command[0], c.Command[1:]...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		_ = master.Close()
		return err
	}
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = master.Close()
		return err
	}
	var nonce [24]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = master.Close()
		return err
	}
	epoch := hex.EncodeToString(nonce[:])
	p := &child{cmd: cmd, master: master, observer: newObserver(c.Columns, c.Rows), state: State{PID: cmd.Process.Pid, Start: start, Epoch: epoch, Running: true, Columns: c.Columns, Rows: c.Rows}, proof: fmt.Sprintf("%d:%s:%s", cmd.Process.Pid, start, epoch), done: make(chan struct{}), readDone: make(chan struct{})}
	h.current = p
	go h.readPTY(p)
	go h.reap(p)
	return nil
}
func (h *Host) readPTY(p *child) {
	defer close(p.readDone)
	data := make([]byte, maxInput)
	for {
		n, err := p.master.Read(data)
		if n > 0 {
			raw := append([]byte(nil), data[:n]...)
			h.mu.Lock()
			p.observer.write(raw)
			for s := range h.subscribers {
				if s.child == p {
					select {
					case s.frames <- Frame{Type: "output", Data: raw}:
					default:
						h.dropLocked(s)
					}
				}
			}
			h.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
func (h *Host) reap(p *child) {
	// WNOWAIT оставляет PID закреплённым до взятия mutex. stop/Close не могут
	// послать сигнал новой группе с переиспользованным PID между проверкой и kill.
	var info unix.Siginfo
	var err error
	for {
		err = unix.Waitid(unix.P_PID, p.state.PID, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			break
		}
	}
	if err == nil {
		err = h.finishGroup(p.state.PID)
	}
	// Вычитываем финальный вывод, но потомок с открытым slave не держит host.
	select {
	case <-p.readDone:
	case <-time.After(200 * time.Millisecond):
		_ = p.master.Close()
		<-p.readDone
	}
	h.mu.Lock()
	if err == nil {
		_ = p.cmd.Wait()
		p.state.Running = false
	} else {
		// Не освобождаем PID и не разрешаем новый сеанс при неподтверждённой очистке.
		p.exitErr = fmt.Errorf("PTY session cleanup is unconfirmed: %w", err)
	}
	_ = p.master.Close()
	final := Frame{Type: "closed"}
	if p.exitErr != nil {
		final = Frame{Type: "error", Data: []byte(p.exitErr.Error())}
	}
	for s := range h.subscribers {
		if s.child == p {
			select {
			case s.frames <- final:
			default:
				h.dropLocked(s)
			}
		}
	}
	p.observer.close()
	close(p.done)
	h.mu.Unlock()
}
func (h *Host) dropLocked(s *subscription) {
	delete(h.subscribers, s)
	if h.writer == s {
		h.writer = nil
	}
	_ = s.conn.Close()
}
func (h *Host) stateLocked() State {
	if h.current == nil {
		return State{}
	}
	return h.current.state
}
func (h *Host) replyLocked() Reply {
	r := Reply{OK: true, Outcome: "complete", State: h.stateLocked()}
	if h.current != nil && h.current.exitErr != nil {
		r.OK, r.Outcome, r.Error = false, "unknown", h.current.exitErr.Error()
		return r
	}
	if r.State.Running {
		r.Proof = h.current.proof
	}
	return r
}
func (h *Host) refusedLocked(err error) Reply {
	r := h.replyLocked()
	if r.Outcome == "unknown" {
		return r
	}
	r.OK = false
	r.Outcome = "refused"
	r.Error = err.Error()
	return r
}
func (h *Host) ownedLocked(proof string) error {
	p := h.current
	if p != nil && p.exitErr != nil {
		return p.exitErr
	}
	if p == nil || !p.state.Running {
		return errors.New("PTY session is not running")
	}
	if proof == "" || proof != p.proof {
		return errors.New("PTY session proof does not match")
	}
	start, err := processStart(p.state.PID)
	if err != nil || start != p.state.Start {
		return errors.New("PTY process ownership cannot be verified")
	}
	return nil
}
func (h *Host) killLocked(p *child) error {
	if err := h.ownedLocked(p.proof); err != nil {
		return err
	}
	return unix.Kill(-p.state.PID, unix.SIGKILL)
}
func (h *Host) resizeLocked(p *child, columns, rows int) error {
	if !validSize(columns, rows) {
		return errors.New("PTY dimensions must be 20..240 columns and 5..80 rows")
	}
	if p.state.Columns == columns && p.state.Rows == rows {
		return nil
	}
	if err := unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(columns), Row: uint16(rows)}); err != nil {
		return err
	}
	p.observer.resize(columns, rows)
	p.state.Columns = columns
	p.state.Rows = rows
	for s := range h.subscribers {
		if s.child == p {
			select {
			case s.frames <- Frame{Type: "resize", Columns: columns, Rows: rows}:
			default:
				h.dropLocked(s)
			}
		}
	}
	return nil
}
func (h *Host) inputLocked(p *child, data []byte) (bool, error) {
	if len(data) == 0 || len(data) > maxInput {
		return false, errors.New("PTY input must contain 1..16384 bytes")
	}
	// Неблокирующая запись не позволяет зависшему приложению удержать mutex.
	n, err := unix.Write(int(p.master.Fd()), data)
	if err != nil {
		return n > 0, fmt.Errorf("PTY input write: %w", err)
	}
	if n != len(data) {
		return n > 0, errors.New("PTY input write incomplete")
	}
	return true, nil
}

// Serve accepts a bounded number of local connections. Cancellation closes the
// listener, attachments and child; attachment cancellation alone never stops it.
func (h *Host) Serve(ctx context.Context, l net.Listener) error {
	h.mu.Lock()
	if h.closing || h.listener != nil {
		h.mu.Unlock()
		return errors.New("PTY host already serving or closed")
	}
	h.listener = l
	h.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = h.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer func() { _ = h.Close(); wg.Wait() }()
	slots := make(chan struct{}, maxConnections)
	for {
		c, err := l.Accept()
		if err != nil {
			h.mu.Lock()
			closed := h.closing
			h.mu.Unlock()
			if closed || ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		h.mu.Lock()
		if h.closing {
			h.mu.Unlock()
			_ = c.Close()
			<-slots
			return nil
		}
		h.connections[c] = true
		h.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots; _ = c.Close(); h.mu.Lock(); delete(h.connections, c); h.mu.Unlock() }()
			h.handle(c)
		}()
	}
}
func (h *Host) handle(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	var request Request
	if err := ReadMessage(br, &request); err != nil {
		_ = WriteMessage(c, Reply{Outcome: "refused", Error: err.Error()})
		return
	}
	h.mu.Lock()
	if h.closing {
		r := h.refusedLocked(errors.New("PTY host is closed"))
		h.mu.Unlock()
		_ = WriteMessage(c, r)
		return
	}
	if request.Action == "attach" {
		h.attachLocked(c, br, request)
		return
	}
	reply := h.requestLocked(request)
	h.mu.Unlock()
	_ = WriteMessage(c, reply)
}
func (h *Host) requestLocked(r Request) Reply {
	fail := func(e error) Reply { return h.refusedLocked(e) }
	switch r.Action {
	case "status":
		return h.replyLocked()
	case "ensure":
		if err := h.startLocked(); err != nil {
			return fail(err)
		}
		return h.replyLocked()
	case "input", "resize", "screen", "tail", "stop":
		if err := h.ownedLocked(r.Proof); err != nil {
			return fail(err)
		}
	default:
		return fail(errors.New("unknown PTY action"))
	}
	p := h.current
	switch r.Action {
	case "screen":
		reply := h.replyLocked()
		reply.Screen = p.observer.terminal.String()
		return reply
	case "tail":
		if r.Lines < 1 || r.Lines > 10000 {
			return fail(errors.New("PTY tail lines must be 1..10000"))
		}
		reply := h.replyLocked()
		reply.Screen = p.observer.tail(r.Lines)
		return reply
	case "input", "resize":
		if h.writer != nil {
			return fail(errors.New("PTY writer conflict"))
		}
		if r.Action == "resize" {
			if err := h.resizeLocked(p, r.Columns, r.Rows); err != nil {
				return fail(err)
			}
		} else {
			effect, err := h.inputLocked(p, r.Data)
			if err != nil {
				reply := fail(err)
				if effect {
					reply.Outcome = "unknown"
				}
				return reply
			}
		}
		return h.replyLocked()
	case "stop":
		if h.writer != nil {
			return fail(errors.New("PTY writer conflict"))
		}
		if err := h.killLocked(p); err != nil {
			return fail(err)
		}
		h.mu.Unlock()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			h.mu.Lock()
			reply := fail(errors.New("PTY stop signaled but exit is unconfirmed"))
			reply.Outcome = "unknown"
			return reply
		}
		h.mu.Lock()
		return h.replyLocked()
	}
	return fail(errors.New("unknown PTY action"))
}
func (h *Host) attachLocked(c net.Conn, br *bufio.Reader, r Request) {
	refuse := func(err error) { reply := h.refusedLocked(err); h.mu.Unlock(); _ = WriteMessage(c, reply) }
	if h.current == nil || !h.current.state.Running {
		refuse(errors.New("PTY session is not running"))
		return
	}
	if h.current.exitErr != nil {
		refuse(h.current.exitErr)
		return
	}
	if r.Proof != "" && r.Proof != h.current.proof {
		refuse(errors.New("PTY session proof does not match"))
		return
	}
	if !r.ReadOnly && h.writer != nil {
		refuse(errors.New("PTY writer conflict"))
		return
	}
	p := h.current
	if !r.ReadOnly && (r.Columns != 0 || r.Rows != 0) {
		if err := h.resizeLocked(p, r.Columns, r.Rows); err != nil {
			refuse(err)
			return
		}
	}
	replay, err := p.observer.replay()
	if err != nil {
		refuse(err)
		return
	}
	s := &subscription{conn: c, frames: make(chan Frame, queueFrames), child: p, readOnly: r.ReadOnly}
	h.subscribers[s] = true
	if !r.ReadOnly {
		h.writer = s
	}
	reply := h.replyLocked()
	reply.Replay = replay
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.dropLocked(s); h.mu.Unlock() }()
	if err = WriteMessage(c, reply); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	done := make(chan struct{})
	go func() { defer close(done); h.readAttachment(s, br) }()
	defer func() { _ = c.Close(); <-done }()
	forwardFrames(c, s.frames, done)
}

// forwardFrames пишет кадры подписки до терминального кадра, ошибки записи
// или выхода читателя attach (done).
func forwardFrames(c net.Conn, frames <-chan Frame, done <-chan struct{}) {
	for {
		select {
		case <-done:
			// Читатель ставит отказ в очередь до выхода, а select выбирает готовый
			// done наравне с ним: без дренажа наблюдатель получал EOF вместо
			// причины (#239). Снимок длины ограничивает дренаж кадрами, уже
			// стоявшими в очереди, хотя readPTY ещё может добавлять вывод; один
			// writeTimeout на весь дренаж, а не на кадр, ограничивает его время.
			_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
			for n := len(frames); n > 0; n-- {
				if !sendFrame(c, <-frames) {
					return
				}
			}
			return
		case frame := <-frames:
			_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
			if !sendFrame(c, frame) {
				return
			}
		}
	}
}

// sendFrame пишет кадр и сообщает, продолжать ли поток: после ошибки записи
// и после терминального кадра соединение закрывается.
func sendFrame(c net.Conn, frame Frame) bool {
	return WriteMessage(c, frame) == nil && frame.Type != "closed" && frame.Type != "error"
}
func (h *Host) readAttachment(s *subscription, br *bufio.Reader) {
	for {
		var frame Frame
		if err := ReadMessage(br, &frame); err != nil {
			return
		}
		h.mu.Lock()
		var err error
		if !h.subscribers[s] || s.readOnly || h.writer != s {
			err = errors.New("PTY attachment is not the writer")
		} else if err = h.ownedLocked(s.child.proof); err == nil {
			switch frame.Type {
			case "input":
				_, err = h.inputLocked(s.child, frame.Data)
			case "resize":
				err = h.resizeLocked(s.child, frame.Columns, frame.Rows)
			default:
				err = errors.New("unknown PTY frame type")
			}
		}
		if err != nil {
			select {
			case s.frames <- Frame{Type: "error", Data: []byte(err.Error())}:
			default:
				h.dropLocked(s)
			}
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
	}
}

// Close idempotently stops the owned process group and closes all connections.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closing = true
		if h.listener != nil {
			_ = h.listener.Close()
		}
		for c := range h.connections {
			_ = c.Close()
		}
		p := h.current
		if p != nil && p.state.Running {
			h.closeErr = h.killLocked(p)
			_ = p.master.Close()
		}
		h.mu.Unlock()
		if p != nil {
			select {
			case <-p.done:
				h.mu.Lock()
				h.closeErr = errors.Join(h.closeErr, p.exitErr)
				h.mu.Unlock()
			case <-time.After(3 * time.Second):
				h.closeErr = errors.New("PTY close could not confirm child exit")
			}
		}
		close(h.closed)
	})
	<-h.closed
	return h.closeErr
}
