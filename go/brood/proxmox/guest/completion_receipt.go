//go:build linux

package guest

import (
	"bufio"
	"context"
	"net"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

func (b *SwarmBroker) forwardCompletionReceipt(connection net.Conn, reader *bufio.Reader, session *brokerSession, request relay.SwarmRequest, response relay.SwarmResponse) {
	receipt, present := relay.CompletionReceiptForResponse(request, response)
	if !present || session == nil {
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	frame, err := reader.ReadSlice('\n')
	if err != nil || !relay.ValidCompletionReceiptFrame(frame, receipt) {
		return
	}
	b.mu.Lock()
	current := b.session == session
	b.mu.Unlock()
	if !current || session.ctx.Err() != nil || b.ctx.Err() != nil {
		return
	}
	ack, err := relay.CompletionReceiptAck(receipt)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	if ctx.Err() != nil {
		return
	}
	_ = session.call(ctx, ack)
}
