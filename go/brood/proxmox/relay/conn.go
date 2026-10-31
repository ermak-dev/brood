package relay

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type deadline struct {
	mu      sync.Mutex
	at      time.Time
	changed chan struct{}
}

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at = t
	if d.changed != nil {
		close(d.changed)
	}
	d.changed = make(chan struct{})
}
func (d *deadline) snapshot() (time.Time, <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.changed == nil {
		d.changed = make(chan struct{})
	}
	return d.at, d.changed
}

type readResult struct {
	data []byte
	err  error
}

// channelConn supplies net.Conn deadlines and CloseWrite for an SSH channel.
// Read timeouts are resettable. A write timeout closes the stream, because SSH
// cannot cancel an in-flight channel write without closing that channel.
type channelConn struct {
	ch              ssh.Channel
	local, remote   net.Addr
	done            chan struct{}
	once            sync.Once
	release         func()
	rd, wd          deadline
	readMu, writeMu sync.Mutex
	reads           chan readResult
	buffer          []byte
	readErr         error
}

func newChannelConn(ctx, peerCtx context.Context, ch ssh.Channel, local, remote net.Addr, release func()) *channelConn {
	c := &channelConn{ch: ch, local: local, remote: remote, done: make(chan struct{}), reads: make(chan readResult), release: release}
	go func() {
		for {
			b := make([]byte, 32<<10)
			n, err := ch.Read(b)
			select {
			case c.reads <- readResult{b[:n], err}:
			case <-c.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-peerCtx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	return c
}
func timerFor(t time.Time) (<-chan time.Time, func()) {
	if t.IsZero() {
		return nil, func() {}
	}
	timer := time.NewTimer(time.Until(t))
	return timer.C, func() { timer.Stop() }
}
func (c *channelConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		select {
		case <-c.done:
			return 0, net.ErrClosed
		default:
		}
		at, changed := c.rd.snapshot()
		if !at.IsZero() && !time.Now().Before(at) {
			return 0, os.ErrDeadlineExceeded
		}
		if len(c.buffer) > 0 {
			n := copy(p, c.buffer)
			c.buffer = c.buffer[n:]
			return n, nil
		}
		if c.readErr != nil {
			return 0, c.readErr
		}
		timer, stop := timerFor(at)
		select {
		case <-changed:
			stop()
			continue
		case <-timer:
			stop()
			return 0, os.ErrDeadlineExceeded
		case <-c.done:
			stop()
			return 0, net.ErrClosed
		case r := <-c.reads:
			stop()
			c.buffer = r.data
			c.readErr = r.err
		}
	}
}
func (c *channelConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	total := 0
	for len(p) > 0 {
		at, _ := c.wd.snapshot()
		if !at.IsZero() && !time.Now().Before(at) {
			return total, os.ErrDeadlineExceeded
		}
		select {
		case <-c.done:
			return total, net.ErrClosed
		default:
		}
		chunk := append([]byte(nil), p[:min(len(p), 32<<10)]...)
		type result struct {
			n   int
			err error
		}
		out := make(chan result, 1)
		go func() { n, err := c.ch.Write(chunk); out <- result{n, err} }()
		for {
			at, changed := c.wd.snapshot()
			timer, stop := timerFor(at)
			select {
			case <-changed:
				stop()
				continue
			case <-timer:
				stop()
				c.Close()
				return total, os.ErrDeadlineExceeded
			case <-c.done:
				stop()
				return total, net.ErrClosed
			case r := <-out:
				stop()
				total += r.n
				p = p[r.n:]
				if r.err != nil {
					return total, r.err
				}
				if r.n == 0 {
					return total, io.ErrShortWrite
				}
			}
			break
		}
	}
	return total, nil
}
func (c *channelConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		// A close packet can itself block on the SSH transport. Keep this
		// stream's slot reserved until it finishes, but unblock local I/O now.
		go func() { c.ch.Close(); c.release() }()
	})
	return nil
}
func (c *channelConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ch.CloseWrite()
}
func (c *channelConn) LocalAddr() net.Addr                { return c.local }
func (c *channelConn) RemoteAddr() net.Addr               { return c.remote }
func (c *channelConn) SetDeadline(t time.Time) error      { c.rd.set(t); c.wd.set(t); return nil }
func (c *channelConn) SetReadDeadline(t time.Time) error  { c.rd.set(t); return nil }
func (c *channelConn) SetWriteDeadline(t time.Time) error { c.wd.set(t); return nil }

var _ net.Conn = (*channelConn)(nil)
