//go:build linux

package guest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

func TestSwarmBrokerUsesCurrentConnectionAndRefusesSpoofing(t *testing.T) {
	if os.Geteuid() != 1000 {
		t.Skip("Unix peer fixture requires actor UID 1000")
	}
	path := filepath.Join(t.TempDir(), "swarm.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	broker := newSwarmBroker(context.Background(), listener, nil)
	defer broker.Close()
	ctx, cancel := context.WithCancel(context.Background())
	release := broker.bind(ctx, func(_ context.Context, r relay.SwarmRequest) relay.SwarmResponse {
		return relay.SwarmResponse{OK: true, Text: r.Tool + " accepted"}
	})
	defer release()
	got := callSwarmBroker(context.Background(), path, relay.SwarmRequest{Tool: "agents"})
	if !got.OK || got.Text != "agents accepted" {
		t.Fatalf("response: %v", got)
	}
	cancel()
	if got := callSwarmBroker(context.Background(), path, relay.SwarmRequest{Tool: "agents"}); got.OK {
		t.Fatal("stale connection used")
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte("{\"tool\":\"send\",\"to\":\"peer\",\"message\":\"hello\",\"from\":\"other\"}\n"))
	var buf [4096]byte
	n, _ := c.Read(buf[:])
	if n == 0 {
		t.Fatal("malformed request needs a bounded refusal")
	}
}
