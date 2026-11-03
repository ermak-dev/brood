//go:build linux

package ccmsg

// FEATURE: Реальный непривилегированный канал с обратными квитанциями.
// SPEC: bypass, как у получателя (#148), отдельный обратный сокет, совпадающий orig_msg_id,
// отсутствие квитанции означает unconfirmed, held не означает delivered,
// обратный notify_when_idle аутентифицирован, ввод и ожидание ограничены.
// IMPLEMENTATION: Реальные Unix fixtures, peer auth, correlation и bypass.
// RESULT: IMPLEMENTED — транспорт квитанций, без модельных вызовов.
// STATUS: DONE

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestExchangeReceipts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("transport intentionally refuses root")
	}
	for _, status := range []string{"delivered", "held", "denied", "refused", "expired", "expired-refused", "dropped", "", "wrong-id"} {
		t.Run(status, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "42.sock")
			l, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			done := make(chan error, 1)
			go func() {
				c, err := l.Accept()
				if err != nil {
					done <- err
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				s := bufio.NewScanner(c)
				if !s.Scan() {
					done <- s.Err()
					return
				}
				var auth Frame
				_ = json.Unmarshal(s.Bytes(), &auth)
				if auth["type"] != "auth" || auth["token"] != "peer-key" {
					t.Errorf("auth: %#v", auth)
				}
				if !s.Scan() {
					done <- s.Err()
					return
				}
				var msg Frame
				_ = json.Unmarshal(s.Bytes(), &msg)
				content := msg["message"].(map[string]any)["content"].(string)
				if !strings.Contains(content, `from-name="lead" from-mode="bypass"`) {
					t.Errorf("envelope: %s", content)
				}
				from := strings.TrimPrefix(msg["from"].(string), "uds:")
				if filepath.Dir(from) != filepath.Dir(path) {
					t.Error("callback outside session socket directory")
				}
				// CLI 2.1.310 пишет квитанцию только для канонического UUID.
				if status != "" && canonicalUUID.MatchString(msg["msg_id"].(string)) {
					cb, err := net.Dial("unix", from)
					if err != nil {
						done <- err
						return
					}
					id := msg["msg_id"]
					got := status
					if status == "wrong-id" {
						id = "unrelated"
						got = "delivered"
					}
					receipt := Frame{"type": "control", "action": "peer_message_status", "orig_msg_id": id, "status": got}
					if status == "expired-refused" {
						receipt["status"], receipt["status_detail"] = "expired", "refused"
					}
					err = json.NewEncoder(cb).Encode(receipt)
					cb.Close()
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := Exchange(ctx, path, "peer-key", "lead", "hello", 0)
			if err != nil {
				t.Fatal(err)
			}
			want := status
			if want == "" || want == "wrong-id" {
				want = "unconfirmed"
			}
			if want == "expired-refused" {
				want = "refused"
			}
			if result.Status != want || result.Idle {
				t.Fatalf("receipt: %#v, want %s", result, want)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(filepath.Dir(path))
			if len(entries) != 1 {
				t.Fatalf("callback leaked: %v", entries)
			}
		})
	}
}

func TestExchangeIdleDoesNotProveDelivery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("transport intentionally refuses root")
	}
	path := filepath.Join(t.TempDir(), "42.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		s := bufio.NewScanner(c)
		var frame Frame
		for i := 0; i < 3; i++ {
			if !s.Scan() {
				done <- s.Err()
				return
			}
			_ = json.Unmarshal(s.Bytes(), &frame)
		}
		if frame["action"] != "notify_when_idle" || frame["from_mode"] != "bypass" {
			t.Errorf("subscription: %#v", frame)
		}
		if !canonicalUUID.MatchString(frame["msg_id"].(string)) {
			done <- nil
			return
		}
		cb, err := net.Dial("unix", strings.TrimPrefix(frame["from"].(string), "uds:"))
		if err != nil {
			done <- err
			return
		}
		defer cb.Close()
		done <- json.NewEncoder(cb).Encode(Frame{"type": "control", "action": "peer_idle_notice", "orig_msg_id": frame["msg_id"], "state": "idle"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := Exchange(ctx, path, "peer-key", "lead", "hello", 1)
	if err != nil || result.Status != "unconfirmed" || !result.Idle {
		t.Fatalf("idle must not mean delivered: %#v %v", result, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExchangeRejectsInvalidInputBeforeDial(t *testing.T) {
	for _, tc := range []struct {
		body string
		wait int
	}{{"", 0}, {strings.Repeat("x", 128*1024), 0}, {"hello", -1}, {"hello", 301}} {
		if _, err := Exchange(context.Background(), "/does/not/exist.sock", "key", "lead", tc.body, tc.wait); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Exchange(ctx, "/does/not/exist.sock", "key", "lead", "hello", 0); err == nil {
		t.Fatal("canceled exchange accepted")
	}
}
