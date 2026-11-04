//go:build linux

package ptysession

import (
	"bufio"
	"net"
	"strings"
	"testing"
)

// BUG: Attach с прежним размером сбрасывал scroll region до replay.
// SPEC: Неизменная геометрия не должна менять состояние терминала при reconnect.
func TestSameSizeWriterAttachRetainsScrollRegion(t *testing.T) {
	const original = "\x1b[2;20r\x1b[?6h\x1b[3;4Htext"
	_, path := fixture(t, original)
	state := mustOK(t, rpc(t, path, Request{Action: "ensure"}))
	awaitScreen(t, path, state.Proof, "text")
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteMessage(conn, Request{Action: "attach", Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	var reply Reply
	if err := ReadMessage(bufio.NewReader(conn), &reply); err != nil {
		t.Fatal(err)
	}
	mustOK(t, reply)
	if !strings.Contains(string(reply.Replay), "\x1b[2;20r") {
		t.Fatalf("unchanged-size attach lost scroll region: %q", reply.Replay)
	}
	cursor := libvtermCursorOracle(t)
	const following = "\x1b[2;5Hx"
	if got, want := cursor(string(reply.Replay)+following), cursor(original+following); got != want {
		t.Fatalf("following live CUP after reconnect: got %s want %s", got, want)
	}
}
