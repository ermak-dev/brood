package relay

import (
	"strings"
	"sync/atomic"
	"testing"
)

func TestGuestCannotChooseServerDeliveryIdentityForRoutedMessage(t *testing.T) {
	f := setup(t)
	var routed atomic.Int32
	f.h.SetMessageRouter(func(string, MessageRequest) MessageResponse {
		routed.Add(1)
		return MessageResponse{OK: true, Verdict: "delivered"}
	})
	conn, _ := rawSSH(t, f, testToken)
	_, err := sendMessage(t, f, conn, MessageRequest{JournalID: strings.Repeat("a", 64), Replay: true, DeliveryID: "server:step:reserved", To: "worker", From: "claimed", Body: "work"})
	if err == nil || routed.Load() != 0 {
		t.Fatalf("guest claimed server delivery identity: routed=%d error=%v", routed.Load(), err)
	}
	response, err := sendMessage(t, f, conn, MessageRequest{To: "worker", From: "peer", Body: "ordinary message"})
	if err != nil || !response.OK || routed.Load() != 1 {
		t.Fatalf("ordinary peer routing changed: %+v %v", response, err)
	}
}
