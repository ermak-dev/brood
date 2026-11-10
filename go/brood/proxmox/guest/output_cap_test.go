package guest

import (
	"io"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// plainReader hides WriterTo, so io.Copy goes through the destination's ReadFrom
// the way os/exec copies a process pipe.
type plainReader struct{ io.Reader }

func TestSessionOutputCapHoldsUnderCopy(t *testing.T) {
	canceled := false
	out := &sessionOutput{cancel: func() { canceled = true }}
	_, err := io.Copy(out, plainReader{strings.NewReader(strings.Repeat("x", relay.MaxSessionBytes+1024))})
	if err == nil || !canceled {
		t.Fatalf("copy err=%v canceled=%v, want size-limit failure and cancel", err, canceled)
	}
	if got := len(out.Bytes()); got > relay.MaxSessionBytes {
		t.Fatalf("buffered %d bytes, limit %d", got, relay.MaxSessionBytes)
	}
}

func TestTailBufferKeepsTailUnderCopy(t *testing.T) {
	tail := &tailBuffer{limit: 16}
	data := strings.Repeat("a", 1000) + "0123456789abcdef"
	if _, err := io.Copy(tail, plainReader{strings.NewReader(data)}); err != nil {
		t.Fatal(err)
	}
	if got := string(tail.Bytes()); got != "0123456789abcdef" {
		t.Fatalf("tail = %q, want last 16 bytes", got)
	}
}
