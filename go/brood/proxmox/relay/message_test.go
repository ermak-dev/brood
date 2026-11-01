// FEATURE: Канал сообщений роя поверх гостевого relay (эпик #113, слой 2).
// SPEC: тип SSH-канала brood-swarm-msg отделён от терминала; ExtraData несёт
// один NDJSON-кадр запроса с пределом размера; ответ — кадр вердикта в канале;
// терминальный канал не меняется.
// STATUS: RED — ждёт хаб-клиентских частей и интеграции.
package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageRequestContract(t *testing.T) {
	raw := `{"to":"claude","body":"hello","from":"worker-2","waitSec":30}`
	var req MessageRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if req.To != "claude" || req.From != "worker-2" || req.WaitSec != 30 {
		t.Fatalf("request: %#v", req)
	}
	// Ответ — плоский: один булев флаг и текст вердикта.
	resp, err := json.Marshal(MessageResponse{OK: true, Verdict: "delivered"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != `{"ok":true,"verdict":"delivered"}` {
		t.Fatalf("response encoding: %s", resp)
	}
}

func TestEncodeDecodeMessageFrame(t *testing.T) {
	frame, err := encodeMessageFrame(MessageRequest{To: "a", Body: "b", From: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) > MaxMessageBytes {
		t.Fatalf("frame within limit: %d", len(frame))
	}
	got, err := decodeMessageFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.To != "a" || got.Body != "b" || got.From != "c" {
		t.Fatalf("roundtrip: %#v", got)
	}
	if _, err := decodeMessageFrame([]byte("not json")); err == nil {
		t.Fatal("garbage must be rejected")
	}
	if _, err := decodeMessageFrame([]byte(`{"to":"","body":"","from":""}`)); err == nil {
		t.Fatal("empty names and body must be rejected")
	}
	oversize := []byte(`{"to":"a","from":"b","body":"` + strings.Repeat("x", MaxMessageBytes) + `"}`)
	if _, err := decodeMessageFrame(oversize); err == nil {
		t.Fatal("oversize frame must be rejected")
	}
}
