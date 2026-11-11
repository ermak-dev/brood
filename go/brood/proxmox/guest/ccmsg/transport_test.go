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
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/command"
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
			result, err := Exchange(ctx, path, "peer-key", "lead", "hello", 0, time.Second)
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
	result, err := Exchange(ctx, path, "peer-key", "lead", "hello", 1, command.ReceiptWait)
	if err != nil || result.Status != "unconfirmed" || !result.Idle {
		t.Fatalf("idle must not mean delivered: %#v %v", result, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExchangeRejectsInvalidInputBeforeDial(t *testing.T) {
	for _, tc := range []struct {
		body    string
		wait    int
		receipt time.Duration
	}{{"", 0, time.Second}, {strings.Repeat("x", 128*1024), 0, time.Second}, {"hello", -1, time.Second}, {"hello", 301, time.Second}, {"hello", 0, 0}, {"hello", 0, 301 * time.Second}} {
		if _, err := Exchange(context.Background(), "/does/not/exist.sock", "key", "lead", tc.body, tc.wait, tc.receipt); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Exchange(ctx, "/does/not/exist.sock", "key", "lead", "hello", 0, time.Second); err == nil {
		t.Fatal("canceled exchange accepted")
	}
}

// TestExchangeNotSentBeforeTheWrite — отказ до записи кадра в inbox
// (неверный ввод, отмена, нет обратного сокета, inbox не принимает
// соединение) — ErrNotSent: сессия сообщения не получила, повтор безопасен
// (#244). После записи ошибка ErrNotSent не несёт: исход неизвестен.
func TestExchangeNotSentBeforeTheWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("transport intentionally refuses root")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	missing := filepath.Join(t.TempDir(), "42.sock")
	for name, call := range map[string]func() error{
		"invalid input": func() error {
			_, err := Exchange(context.Background(), missing, "key", "lead", "hello", 301, time.Second)
			return err
		},
		"invalid receipt wait": func() error {
			_, err := Exchange(context.Background(), missing, "key", "lead", "hello", 0, 0)
			return err
		},
		"canceled": func() error {
			_, err := Exchange(canceled, missing, "key", "lead", "hello", 0, time.Second)
			return err
		},
		"no callback directory": func() error {
			_, err := Exchange(context.Background(), "/does/not/exist.sock", "key", "lead", "hello", 0, time.Second)
			return err
		},
		"inbox not listening": func() error {
			_, err := Exchange(context.Background(), missing, "key", "lead", "hello", 0, time.Second)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrNotSent) {
			t.Errorf("%s: %v is not ErrNotSent", name, err)
		}
	}

	path := filepath.Join(t.TempDir(), "42.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Кадры записаны и прочитаны, квитанции нет: отмена ожидания.
		_, _ = io.Copy(io.Discard, c)
		stop()
	}()
	if _, err := Exchange(ctx, path, "peer-key", "lead", "hello", 1, time.Second); err == nil || errors.Is(err, ErrNotSent) {
		t.Fatalf("written message classified as not sent: %v", err)
	}
}

// TestExchangeWaitsForALateReceipt — Claude Code присылает квитанцию не
// сразу (#260): без ожидания простоя Exchange ждёт её до предела и
// возвращается сразу по получении, а не по истечении предела.
func TestExchangeWaitsForALateReceipt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("transport intentionally refuses root")
	}
	for _, tc := range []struct {
		status, want string
		after        time.Duration
	}{{"delivered", "delivered", 2 * time.Second}, {"refused", "refused", time.Second}} {
		t.Run(tc.status, func(t *testing.T) {
			path := lateReceiptInbox(t, tc.status, tc.after)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			began := time.Now()
			result, err := Exchange(ctx, path, "peer-key", "lead", "hello", 0, command.ReceiptWait)
			took := time.Since(began)
			if err != nil || result.Status != tc.want || result.Idle {
				t.Fatalf("late receipt: %#v %v", result, err)
			}
			if took < tc.after || took > tc.after+2*time.Second {
				t.Fatalf("returned after %v, receipt sent after %v", took, tc.after)
			}
		})
	}
}

// lateReceiptInbox — inbox сессии, который присылает квитанцию status на
// обратный сокет через after после записи сообщения.
func lateReceiptInbox(t *testing.T, status string, after time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "42.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		s := bufio.NewScanner(c)
		var msg Frame
		for msg["type"] != "user" && s.Scan() {
			msg = Frame{}
			_ = json.Unmarshal(s.Bytes(), &msg)
		}
		time.Sleep(after)
		cb, err := net.Dial("unix", strings.TrimPrefix(msg["from"].(string), "uds:"))
		if err != nil {
			return
		}
		defer cb.Close()
		_ = json.NewEncoder(cb).Encode(Frame{"type": "control", "action": "peer_message_status", "orig_msg_id": msg["msg_id"], "status": status})
	}()
	return path
}

// TestExchangeReceiptWaitIsBounded — квитанции нет: без ожидания простоя
// Exchange ждёт её ровно до предела и отвечает unconfirmed без ошибки
// канала (#260). Ожидание простоя (waitSec > 0) предел квитанции не
// удлиняет: оно кончается через waitSec, как прежде.
func TestExchangeReceiptWaitIsBounded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("transport intentionally refuses root")
	}
	for _, tc := range []struct {
		name    string
		wait    int
		receipt time.Duration
		limit   time.Duration
	}{{"receipt", 0, time.Second, time.Second}, {"idle", 1, command.ReceiptWait, time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			path := silentInbox(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			began := time.Now()
			result, err := Exchange(ctx, path, "peer-key", "lead", "hello", tc.wait, tc.receipt)
			took := time.Since(began)
			if err != nil || result.Status != "unconfirmed" || result.Idle {
				t.Fatalf("no receipt: %#v %v", result, err)
			}
			if took < tc.limit || took > tc.limit+2*time.Second {
				t.Fatalf("returned after %v, limit %v", took, tc.limit)
			}
		})
	}
}

// silentInbox — inbox сессии, который читает кадры и ничего не отвечает.
func silentInbox(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "42.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
	}()
	return path
}
