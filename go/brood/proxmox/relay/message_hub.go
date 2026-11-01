package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/crypto/ssh"
)

// MessageRouter применяет запрос гостя к соседу по рою. UID — уже
// авторизованный отправитель; роутер проверяет рой и адрес на стороне
// сервера и возвращает вердикт адресату. Реализация обязана быть
// bounded: запрос не больше MaxMessageBytes, ожидание задаёт гость.
type MessageRouter func(fromUID string, req MessageRequest) MessageResponse

// serveMessage обрабатывает один канал brood-swarm-msg: запрос лежит в
// ExtraData, вердикт возвращается одним JSON-объектом в канал.
func serveMessage(router MessageRouter, fromUID string, ch ssh.NewChannel) {
	if len(ch.ExtraData()) == 0 || len(ch.ExtraData()) > MaxMessageBytes {
		ch.Reject(ssh.Prohibited, "invalid swarm message frame")
		return
	}
	req, err := decodeMessageFrame(ch.ExtraData())
	if err != nil {
		ch.Reject(ssh.Prohibited, err.Error())
		return
	}
	stream, reqs, err := ch.Accept()
	if err != nil {
		return
	}
	defer stream.Close()
	go ssh.DiscardRequests(reqs)
	resp := router(fromUID, req)
	if !resp.OK && resp.Verdict == "" {
		resp.Verdict = "swarm message refused"
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if len(out) > MaxMessageBytes {
		out, _ = json.Marshal(MessageResponse{OK: false, Verdict: "swarm message verdict too large"})
	}
	_, _ = stream.Write(append(out, '\n'))
}

// readMessageResponse читает вердикт адресата из канала сообщений: один
// JSON-объект с пределом размера и таймаутом на чтение.
func readMessageResponse(r io.Reader) (MessageResponse, error) {
	buf := make([]byte, MaxMessageBytes+1)
	n, err := r.Read(buf)
	if err != nil && n == 0 {
		return MessageResponse{}, fmt.Errorf("swarm message verdict unreadable: %w", err)
	}
	if n > MaxMessageBytes {
		return MessageResponse{}, errMessageTooLarge
	}
	var resp MessageResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return MessageResponse{}, errors.New("invalid swarm message verdict")
	}
	if resp.Verdict == "" {
		return MessageResponse{}, errors.New("empty swarm message verdict")
	}
	return resp, nil
}

// ReadMessageResponse — экспортированная обёртка для сервера-роутера.
func ReadMessageResponse(r io.Reader) (MessageResponse, error) { return readMessageResponse(r) }

// DialMessage открывает канал сообщений в гостя: запрос целиком в ExtraData,
// ответ читается из возвращённого соединения. Контекст ограничивает открытие
// и всё время жизни соединения, как у терминального Dial.
func (h *Hub) DialMessage(ctx context.Context, uid string, generation int64, frame []byte) (net.Conn, error) {
	if len(frame) == 0 || len(frame) > MaxMessageBytes {
		return nil, errMessageTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := h.session(uid, generation)
	if p == nil {
		return nil, errUnavailable
	}
	h.mu.Lock()
	if h.closed || h.peers[p.id] != p || p.streams >= MaxStreams {
		h.mu.Unlock()
		return nil, errUnavailable
	}
	p.streams++
	h.mu.Unlock()
	release := func() { h.mu.Lock(); p.streams--; h.mu.Unlock() }
	type result struct {
		ch  ssh.Channel
		req <-chan *ssh.Request
		err error
	}
	out := make(chan result)
	go func() {
		ch, req, err := p.conn.OpenChannel(messageChannel, frame)
		select {
		case out <- result{ch, req, err}:
		case <-ctx.Done():
			if ch != nil {
				ch.Close()
			}
			release()
		}
	}()
	select {
	case <-ctx.Done():
		p.conn.Close()
		return nil, ctx.Err()
	case r := <-out:
		if r.err != nil {
			release()
			return nil, errUnavailable
		}
		go ssh.DiscardRequests(r.req)
		h.mu.Lock()
		valid := !h.closed && h.peers[p.id] == p && h.authorize(uid, generation, p.token)
		h.mu.Unlock()
		if !valid {
			r.ch.Close()
			release()
			return nil, errUnavailable
		}
		return newChannelConn(ctx, p.ctx, r.ch, p.conn.LocalAddr(), p.conn.RemoteAddr(), release), nil
	}
}
