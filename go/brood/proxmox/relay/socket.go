package relay

import (
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const maxFrame = 64 << 10

var binaryCodec = websocket.Codec{
	Marshal: func(v any) ([]byte, byte, error) { return v.([]byte), websocket.BinaryFrame, nil },
	Unmarshal: func(data []byte, kind byte, v any) error {
		if kind != websocket.BinaryFrame {
			return errors.New("relay requires binary frames")
		}
		*v.(*[]byte) = data
		return nil
	},
}

// socket preserves stream semantics while bounding individual websocket frames.
// Codec.Receive, unlike Conn.Read, enforces MaxPayloadBytes.
type socket struct {
	*websocket.Conn
	readMu, writeMu sync.Mutex
	buffer          []byte
}

func newSocket(c *websocket.Conn) *socket {
	c.PayloadType = websocket.BinaryFrame
	c.MaxPayloadBytes = maxFrame
	return &socket{Conn: c}
}
func (s *socket) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.buffer) == 0 {
		if err := binaryCodec.Receive(s.Conn, &s.buffer); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	return n, nil
}
func (s *socket) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n := 0
	for len(p) > 0 {
		k := min(len(p), 32<<10)
		if err := binaryCodec.Send(s.Conn, p[:k]); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}
func (s *socket) Close() error { s.Conn.SetDeadline(time.Now()); return s.Conn.Close() }

var _ io.ReadWriteCloser = (*socket)(nil)
