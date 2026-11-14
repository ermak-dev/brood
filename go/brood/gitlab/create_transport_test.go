package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
)

// createWriteFailureConn exercises the real HTTP transport's reused-connection
// zero-byte-write retry path, not a mock RoundTripper's retry behavior.
type createWriteFailureConn struct {
	net.Conn
	armed    *atomic.Bool
	failures *atomic.Int32
}

func (c *createWriteFailureConn) Write(p []byte) (int, error) {
	if c.armed.CompareAndSwap(true, false) {
		c.failures.Add(1)
		return 0, errors.New(sentinel + " UPSTREAM_PRIVATE")
	}
	return c.Conn.Write(p)
}

func TestCreateTransportCannotRewindPOST(t *testing.T) {
	var posts, dials, failures atomic.Int32
	var armed atomic.Bool
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"id":42}`)
			return
		}
		posts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		writeCreated(w, createdMR())
	})
	tr := c.http.Transport.(*http.Transport)
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &createWriteFailureConn{Conn: conn, armed: &armed, failures: &failures}, nil
	}
	if _, err := c.Project(context.Background()); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	mr, err := c.CreateMergeRequest(context.Background(), "main", createInput())
	checkCreateError(t, err, Transport, CreateUncertain, 0)
	if mr != (MergeRequest{}) || posts.Load() != 0 || dials.Load() != 1 || failures.Load() != 1 {
		t.Fatalf("transport rewound POST: posts=%d dials=%d failures=%d", posts.Load(), dials.Load(), failures.Load())
	}
}

func TestCreateCallerCancellationAfterSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var posts atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		<-r.Context().Done()
	})
	mr, err := c.CreateMergeRequest(ctx, "main", createInput())
	checkCreateError(t, err, Canceled, CreateUncertain, 0)
	if !errors.Is(err, context.Canceled) || posts.Load() != 1 || mr != (MergeRequest{}) {
		t.Fatal("canceled create replayed or lost effect")
	}
}
