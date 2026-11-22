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
	if req.DeliveryID != "" || req.JournalID != "" || req.Replay {
		ch.Reject(ssh.Prohibited, "delivery IDs are reserved for server instructions")
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

// readMessageResponse читает вердикт адресата из канала сообщений: JSON-объект
// накапливается до перевода строки или EOF — SSH дробит большие записи на
// пакеты, один Read их не собирает. Общий объём ограничен MaxMessageBytes.
func readMessageResponse(r io.Reader) (MessageResponse, error) {
	acc := make([]byte, 0, 1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if len(acc)+n > MaxMessageBytes {
				return MessageResponse{}, errMessageTooLarge
			}
			acc = append(acc, buf[:n]...)
			if idx := lastIndexByte(acc, '\n'); idx >= 0 {
				acc = acc[:idx]
				break
			}
		}
		if err != nil {
			if len(acc) > 0 && (err == io.EOF || err == io.ErrUnexpectedEOF) {
				break
			}
			return MessageResponse{}, fmt.Errorf("swarm message verdict unreadable: %w", err)
		}
	}
	var resp MessageResponse
	if err := json.Unmarshal(acc, &resp); err != nil {
		return MessageResponse{}, errors.New("invalid swarm message verdict")
	}
	if resp.Verdict == "" {
		return MessageResponse{}, errors.New("empty swarm message verdict")
	}
	return resp, nil
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
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
	request, err := decodeMessageFrame(frame)
	if err != nil {
		return nil, err
	}
	// Проверка читает неизменяемые данные рукопожатия выбранного гостя.
	capability := func(p *peer) error {
		if request.DeliveryID == "" {
			return nil
		}
		if p.messageJournalID == "" {
			return ErrMessageDedupUnsupported
		}
		if p.messageJournalID != request.JournalID {
			return ErrDeliveryJournalChanged
		}
		return nil
	}
	p := h.session(uid, generation)
	if p == nil {
		return nil, errUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.closed || h.peers[p.id] != p || p.streams >= MaxStreams {
		h.mu.Unlock()
		return nil, errUnavailable
	}
	if err := capability(p); err != nil {
		h.mu.Unlock()
		return nil, err
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
	openCtx, openCancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer openCancel()
	go func() {
		var ch ssh.Channel
		var req <-chan *ssh.Request
		err := capability(p)
		if err == nil {
			if !h.currentPeer(p) {
				err = errUnavailable
			} else if err = ctx.Err(); err == nil {
				ch, req, err = p.conn.OpenChannel(messageChannel, frame)
			}
		}
		select {
		case out <- result{ch, req, err}:
		case <-openCtx.Done():
			// Открытие — короткая операция с собственным дедлайном:
			// отмена доставки не рвёт SSH-соединение получателя, чьи
			// терминалы и другие доставки здесь ни при чём.
			if ch != nil {
				ch.Close()
			}
			release()
		}
	}()
	select {
	case <-openCtx.Done():
		return nil, errUnavailable
	case r := <-out:
		if r.err != nil {
			release()
			if errors.Is(r.err, ErrMessageDedupUnsupported) {
				return nil, ErrMessageDedupUnsupported
			}
			if errors.Is(r.err, ErrDeliveryJournalChanged) {
				return nil, ErrDeliveryJournalChanged
			}
			if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
				return nil, r.err
			}
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
