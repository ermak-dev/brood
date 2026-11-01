// FEATURE: Канал доставки сообщений в сессию Claude Code (порт mop CHANNEL.md).
// SPEC: конверт cross-session-message с фиксированным порядком атрибутов,
// NDJSON-кадры type:user/auth/control, приоритеты now/next/later, квитанции
// peer_message_status и peer_idle_notice по orig_msg_id, ключ инбокса
// <pid>.<sha256(realpath)>.key, выбор живой сессии каталога.
// IMPLEMENTATION: ccmsg.go — конверт, кадры, NDJSON, квитанции, ключ инбокса
// и выбор живой сессии каталога; сетевые части придут со слоем relay.
// RESULT: IMPLEMENTED — контракт канала mop CHANNEL.md покрыт юнит-тестами.
// STATUS: DONE
package ccmsg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func num(f float64) string { b, _ := json.Marshal(f); return string(b) }

func TestEnvelopeAttributeOrder(t *testing.T) {
	got, err := EnvelopeBuilder{From: "uds:/run/user/1000/cc-socks/42.sock",
		FromName: "claude", FromMode: "bypass"}.Build("hello")
	if err != nil {
		t.Fatal(err)
	}
	want := "<cross-session-message from=\"uds:/run/user/1000/cc-socks/42.sock\" from-name=\"claude\" from-mode=\"bypass\">\nhello\n</cross-session-message>"
	if got != want {
		t.Fatalf("envelope mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestEnvelopeEncodesSpecialsAndRejectsBadValues(t *testing.T) {
	got, err := EnvelopeBuilder{From: "uds:/tmp/a b.sock"}.Build("x")
	if err != nil {
		t.Fatal(err)
	}
	if want := "<cross-session-message from=\"uds:/tmp/a%20b.sock\">\nx\n</cross-session-message>"; got != want {
		t.Fatalf("percent encoding: got %q", got)
	}
	if _, err := (EnvelopeBuilder{FromMode: "sudo"}).Build("x"); err == nil {
		t.Fatal("from-mode must be rejected")
	}
	if _, err := (EnvelopeBuilder{FromName: "a\"b"}).Build("x"); err == nil {
		t.Fatal("quote in from-name must be rejected")
	}
	if _, err := (EnvelopeBuilder{}).Build(""); err == nil {
		t.Fatal("empty body must be rejected")
	}
}

func TestUserFrameDefaultsAndValidation(t *testing.T) {
	f, err := UserFrame("body", "", "uds:x", "id-1")
	if err != nil {
		t.Fatal(err)
	}
	if f["priority"] != "next" {
		t.Fatalf("default priority: %v", f["priority"])
	}
	if f["msgV"] != 1 || f["type"] != "user" || f["from"] != "uds:x" {
		t.Fatalf("frame fields: %#v", f)
	}
	msg := f["message"].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "body" {
		t.Fatalf("message: %#v", msg)
	}
	if _, err := UserFrame("", "next", "", ""); err == nil {
		t.Fatal("empty content must be rejected")
	}
	if _, err := UserFrame("x", "urgent", "", ""); err == nil {
		t.Fatal("unknown priority must be rejected")
	}
}

func TestNDJSONRoundTripAndReceipts(t *testing.T) {
	user, _ := UserFrame("body", "now", "", "m1")
	sub, _ := ControlFrame("notify_when_idle", "uds:me", "bypass", "m2")
	line, err := EncodeNDJSON([]Frame{AuthFrame("tok"), user, sub})
	if err != nil {
		t.Fatal(err)
	}
	frames := DecodeNDJSON(line)
	if len(frames) != 3 {
		t.Fatalf("frame count: %d", len(frames))
	}
	frames = append(frames, DecodeNDJSON("not json\n\n")...)
	if len(frames) != 3 {
		t.Fatalf("garbage must be skipped: %d", len(frames))
	}
	// Квитанции приходят отдельным соединением в наш инбокс.
	receipts := DecodeNDJSON(`{"type":"control","action":"peer_message_status","orig_msg_id":"m1","status":"delivered"}` + "\n" +
		`{"type":"control","action":"peer_idle_notice","orig_msg_id":"m2","state":"idle"}` + "\n")
	frames = append(frames, receipts...)
	status, err := Receipt(frames, "peer_message_status", "m1")
	if err != nil {
		t.Fatal("user receipt expected")
	}
	if status["status"] != "delivered" {
		t.Fatalf("receipt status: %#v", status)
	}
	idle, err := Receipt(frames, "peer_idle_notice", "m2")
	if err != nil {
		t.Fatal("idle receipt expected")
	}
	if idle["state"] != "idle" {
		t.Fatalf("idle state: %#v", idle)
	}
	if _, err := Receipt(frames, "peer_idle_notice", "m1"); err == nil {
		t.Fatal("receipts must match their own msg ids")
	}
}

func TestPeerTokenFindsKeyByRealpathHash(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock", "4242.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	// Сокет-файл существует: realpath живого сокета разрешается и в тесте.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Ключ лежит не рядом с сокетом, а в каталоге сессий, и назван отпечатком
	// canonical пути сокета: symlink на сокет не меняет отпечаток.
	keyName := "4242." + sha256Hex(realPath(t, sock)) + ".key"
	if err := os.WriteFile(filepath.Join(dir, keyName),
		[]byte(`{"peerToken":"32hex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := PeerToken(sock, dir)
	if err != nil {
		t.Fatal(err)
	}
	if token != "32hex" {
		t.Fatalf("token: %q", token)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if token, err := PeerToken(link, dir); err != nil || token != "32hex" {
		t.Fatalf("symlinked socket must resolve to the same key: %v %v", token, err)
	}
	if _, err := PeerToken(filepath.Join(dir, "other.sock"), dir); err == nil {
		t.Fatal("missing key must be an error")
	}
}

func TestFindSessionsNewestFirstAndCWDSelection(t *testing.T) {
	dir := t.TempDir()
	write := func(name, socket string, updated float64, cwd string) {
		body := `{"pid":1,"messagingSocketPath":` + quote(socket) +
			`,"statusUpdatedAt":` + num(updated) + `,"cwd":` + quote(cwd) + `}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10.json", "/run/a.sock", 100, "/workspace/project")
	write("11.json", "/run/b.sock", 200, "/workspace/project")
	// Файл без сокета каналом не интересен, сколько бы их ни было.
	if err := os.WriteFile(filepath.Join(dir, "12.json"), []byte(`{"pid":12}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, err := FindSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].SocketPath != "/run/b.sock" {
		t.Fatalf("sessions: %#v", sessions)
	}
	alive := map[string]bool{"/run/a.sock": true}
	s, err := SessionForCWD(dir, "/workspace/project", func(p string) bool { return alive[p] })
	if err != nil {
		t.Fatal(err)
	}
	if s.SocketPath != "/run/a.sock" {
		t.Fatalf("must fall through dead newest to a live socket: %q", s.SocketPath)
	}
	alive = nil
	if _, err := SessionForCWD(dir, "/workspace/project", func(string) bool { return false }); err == nil {
		t.Fatal("all dead must be an error")
	}
	if _, err := SessionForCWD(dir, "/elsewhere", func(string) bool { return true }); err == nil {
		t.Fatal("no sessions must be an error")
	}
}
