package relay

// BUG: Контекст доставки — ребёнок 30-секундного роутерного контекста:
// WaitSec>~15 с молча урезается, а отмена доставки рвёт всё SSH-соединение
// получателя (терминалы умирают).
// HYPOTHESIS: delivery-контекст нужно выводить от Background с собственным
// потолком, а отмену открытия канала отделять от времени жизни соединения.
import (
	"context"
	"io"
	"testing"
	"time"
)

func TestMessageResponseReadAccumulatesUntilNewline(t *testing.T) {
	// Вердикт крупнее одного SSH-пакета приезжает несколькими Read:
	// читатель обязан собрать его целиком до перевода строки.
	chunk := make([]byte, 33*1024)
	for i := range chunk {
		chunk[i] = 'x'
	}
	half := len(chunk) / 2
	r := &pieceReader{pieces: [][]byte{append([]byte(`{"ok":true,"verdict":"`), chunk[:half]...), append(chunk[half:], []byte(`"}`+"\n")...)}}
	resp, err := readMessageResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Verdict) != len(chunk) {
		t.Fatalf("verdict length: %d", len(resp.Verdict))
	}
}

type pieceReader struct {
	pieces [][]byte
	i      int
}

func (p *pieceReader) Read(b []byte) (int, error) {
	if p.i >= len(p.pieces) {
		return 0, io.EOF
	}
	n := copy(b, p.pieces[p.i])
	p.i++
	return n, nil
}

func TestMessageResponseOversizeAccumulationRefused(t *testing.T) {
	big := append([]byte(`{"ok":true,"verdict":"`), make([]byte, MaxMessageBytes+1024)...)
	r := &pieceReader{pieces: [][]byte{big[:len(big)/2], big[len(big)/2:]}}
	if _, err := readMessageResponse(r); err == nil {
		t.Fatal("oversize verdict must be refused while accumulating")
	}
}

func TestDialMessageCancelKeepsRecipientConnection(t *testing.T) {
	f := setup(t)
	f.h.SetMessageRouter(func(string, MessageRequest) MessageResponse {
		return MessageResponse{OK: true, Verdict: "ok"}
	})
	start(t, f, f.cfg)
	// Терминальный поток живёт до конца теста.
	term := dial(t, f)
	if _, err := term.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Адресат не отвечает на открытие канала сообщений: слушатель занят
	// терминалом и каналы не принимает — отклоняем всё на стороне «гостя»
	// нельзя, поэтому проверяем отмену по таймауту короткого контекста.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	frame, _ := encodeMessageFrame(MessageRequest{To: "a", Body: "b", From: "c"})
	if _, err := f.h.DialMessage(ctx, "actor-uid", 3, frame); err == nil {
		t.Fatal("dial must fail on cancellation")
	}
	// Соединение получателя пережило отмену: терминал по-прежнему работает.
	if _, err := term.Write([]byte("still alive")); err != nil {
		t.Fatalf("recipient connection was killed by a cancelled message dial: %v", err)
	}
}
