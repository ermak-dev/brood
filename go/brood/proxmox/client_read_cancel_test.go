package proxmox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// BUG: CI saw "invalid PVE JSON envelope" instead of cancellation when the
// deadline expired while a partial task body ended without a read error.
// HYPOTHESIS: requestData consults ctx.Err only when io.ReadAll fails.
type cancelOnRead struct {
	body   io.Reader
	cancel context.CancelFunc
}

func (b *cancelOnRead) Read(p []byte) (int, error) {
	b.cancel()
	return b.body.Read(p)
}
func (*cancelOnRead) Close() error { return nil }

type partialBodyTransport struct{ cancel context.CancelFunc }

func (t partialBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := &cancelOnRead{body: strings.NewReader(`{"data":`), cancel: t.cancel}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body, Request: r}, nil
}

func TestCanceledBodyReadKeepsCancellationCause(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("transport bypassed") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.http.Transport = partialBodyTransport{cancel: cancel}
	err := c.WaitTask(ctx, task)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUncertain) {
		t.Fatalf("lost body cancellation identity: %v", err)
	}
}
