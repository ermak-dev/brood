package relay

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// A channel close writes an SSH packet, so a stalled peer can also stall Close.
type stalledChannel struct {
	started    chan struct{}
	closed     chan struct{}
	allowClose chan struct{}
	once       sync.Once
}

func (s *stalledChannel) Read([]byte) (int, error)  { <-s.closed; return 0, io.EOF }
func (s *stalledChannel) Write([]byte) (int, error) { close(s.started); <-s.closed; return 0, io.EOF }
func (s *stalledChannel) Close() error {
	<-s.allowClose
	s.once.Do(func() { close(s.closed) })
	return nil
}
func (s *stalledChannel) CloseWrite() error                              { return nil }
func (s *stalledChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (s *stalledChannel) Stderr() io.ReadWriter                          { return s }

func TestWriteDeadlineDoesNotWaitForSSHClose(t *testing.T) {
	s := &stalledChannel{started: make(chan struct{}), closed: make(chan struct{}), allowClose: make(chan struct{})}
	defer close(s.allowClose)
	c := newChannelConn(context.Background(), context.Background(), s, nil, nil, func() {})
	done := make(chan error, 1)
	go func() { _, err := c.Write([]byte("blocked")); done <- err }()
	<-s.started
	c.SetWriteDeadline(time.Now().Add(-time.Second))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write error %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write deadline waited for SSH close packet")
	}
}
