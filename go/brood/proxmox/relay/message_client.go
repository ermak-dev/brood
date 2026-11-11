package relay

import (
	"context"
	"encoding/json"
	"io"

	"brood.dev/go/brood/command"
	"golang.org/x/crypto/ssh"
)

func serveGuestMessage(parent context.Context, handler func(context.Context, MessageRequest) MessageResponse, ch ssh.NewChannel) {
	req, err := decodeMessageFrame(ch.ExtraData())
	if err != nil {
		ch.Reject(ssh.Prohibited, "invalid swarm message frame")
		return
	}
	// Предел канала — command.CallTimeout: без ожидания простоя гость ждёт
	// квитанцию доставки (#260), и канал не закрывается раньше helper-а.
	ctx, cancel := context.WithTimeout(parent, command.CallTimeout(req.WaitSec))
	defer cancel()
	stream, requests, err := ch.Accept()
	if err != nil {
		return
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	go ssh.DiscardRequests(requests)
	// Запрос целиком в ExtraData: EOF или лишние данные означают конец
	// доставки. Закрытие одного канала не должно оставлять helper на 300 с.
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(stream, 1))
		cancel()
	}()
	response := handler(ctx, req)
	if response.Verdict == "" {
		response = MessageResponse{Verdict: "recipient did not confirm delivery"}
	}
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	if len(data) > MaxMessageBytes {
		data, _ = json.Marshal(MessageResponse{Verdict: "swarm message verdict too large"})
	}
	_, _ = stream.Write(append(data, '\n'))
}
