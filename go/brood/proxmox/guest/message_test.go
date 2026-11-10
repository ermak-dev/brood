//go:build linux

package guest

// FEATURE: Доставка в сессию Claude только после границы UID 1000.
// SPEC: Фиксированный helper текущего immutable бинарника, очищенное окружение,
// без путей сокетов или команд в аргументах, bounded JSON на обеих сторонах.
// IMPLEMENTATION: Фиксированный UID-helper и консервативные вердикты.
// RESULT: IMPLEMENTED — receive path; живой root-to-UID запуск не проверен.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest/ccmsg"
	"brood.dev/go/brood/proxmox/relay"
)

func TestDeliveryVerdictsNeverInferDelivery(t *testing.T) {
	for _, tc := range []struct {
		status  string
		idle    bool
		wait    int
		ok      bool
		verdict string
	}{
		{"unconfirmed", false, 0, false, "submitted; delivery unconfirmed"},
		{"unconfirmed", true, 1, false, "submitted; delivery unconfirmed; idle confirmed"},
		{"delivered", false, 0, true, "delivered"},
		{"delivered", false, 1, true, "delivered; idle not confirmed"},
		{"held", false, 1, false, "held"},
		{"denied", false, 0, false, "denied"},
	} {
		got := deliveryResponse(ccmsg.Delivery{Status: tc.status, Idle: tc.idle}, tc.wait)
		if got.OK != tc.ok || got.Verdict != tc.verdict {
			t.Fatalf("%#v: %#v", tc, got)
		}
	}
}

func TestMessageHelperSpec(t *testing.T) {
	s := messageHelperSpec()
	if s.Executable != "/proc/self/exe" || s.UID != 1000 || s.GID != 1000 || s.Dir != "/workspace/project" || !reflect.DeepEqual(s.Args, []string{"--brood-guest-message"}) {
		t.Fatalf("unsafe helper: %#v", s)
	}
	for _, env := range s.Env {
		if strings.Contains(env, "BROOD_GUEST_") || strings.Contains(env, "TOKEN=") {
			t.Fatal("root secret in helper environment")
		}
	}
	wantEnv := []string{"HOME=/workspace/home", "PWD=/workspace/project", "USER=claude", "LOGNAME=claude", "PATH=/usr/local/bin:/usr/bin:/bin", "GOMAXPROCS=4"}
	if !reflect.DeepEqual(s.Env, wantEnv) {
		t.Fatal("helper environment contains settings beyond its fixed minimum")
	}
}

func TestMessageHelperInputValidation(t *testing.T) {
	for _, input := range []string{"", `{}`, `{"to":"a","from":"b","body":"c","waitSec":301}`, `{"to":"a","from":"b","body":"c","path":"/root/service.sock"}`, `{"to":"a","from":"b","body":"c"} {}`, strings.Repeat("x", relay.MaxMessageBytes+1)} {
		if _, err := ReadDeliveryRequest(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid input accepted: %.100s", input)
		}
	}
	req := relay.MessageRequest{To: "worker", From: "lead", Body: "hello"}
	data, _ := json.Marshal(req)
	got, err := ReadDeliveryRequest(strings.NewReader(string(data)))
	if err != nil || got != req {
		t.Fatalf("request: %#v %v", got, err)
	}
}

func TestReceiveMessagePrivilegeGate(t *testing.T) {
	if os.Getuid() == 1000 && os.Geteuid() == 1000 && os.Getgid() == 1000 && os.Getegid() == 1000 {
		t.Skip("requires an identity other than the actor")
	}
	if got := ReceiveMessage(context.Background(), relay.MessageRequest{To: "worker", From: "lead", Body: "hello"}); got.OK || !got.NotSent || got.Verdict != "message helper requires UID and GID 1000" {
		t.Fatalf("privilege gate: %#v", got)
	}
}

func TestDeliverMessageRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root supervisor only")
	}
	got := DeliverMessage(context.Background(), relay.MessageRequest{To: "a", From: "b", Body: "c"})
	if got.OK || !got.NotSent || got.Verdict != "message delivery requires guest supervisor" {
		t.Fatalf("root gate: %#v", got)
	}
}

// TestReceiveMessageClassifiesRefusals — отказ helper-а до записи в сессию
// (личность, нет единственной живой сессии, нет ключа peer, Exchange отказал
// до записи) помечен NotSent: сервер повторит доставку (#244). Ошибка
// Exchange после записи и неподтверждённая запись — неизвестный исход без
// NotSent. Шаги после отказа не выполняются.
func TestReceiveMessageClassifiesRefusals(t *testing.T) {
	session := ccmsg.Session{SocketPath: "/workspace/home/.claude/sessions/42.sock"}
	type step struct {
		identity, session, token error
		exchange                 ccmsg.Delivery
		exchangeErr              error
	}
	for _, tc := range []struct {
		name    string
		step    step
		calls   string
		ok      bool
		notSent bool
		verdict string
	}{
		{"identity", step{identity: errors.New("message helper has unexpected supplementary groups")}, "i", false, true, "message helper has unexpected supplementary groups"},
		{"no live session", step{session: errors.New("no live session")}, "is", false, true, "no unique live Claude session for this project"},
		{"no peer key", step{token: errors.New("missing key")}, "ist", false, true, "Claude session peer key unavailable"},
		{"exchange before the write", step{exchangeErr: fmt.Errorf("session inbox unavailable: %w", ccmsg.ErrNotSent)}, "iste", false, true, "Claude session message was not sent"},
		{"exchange after the write", step{exchangeErr: context.DeadlineExceeded}, "iste", false, false, "Claude session message exchange failed"},
		{"unconfirmed", step{exchange: ccmsg.Delivery{Status: "unconfirmed"}}, "iste", false, false, "submitted; delivery unconfirmed"},
		{"held", step{exchange: ccmsg.Delivery{Status: "held"}}, "iste", false, false, "held"},
		{"delivered", step{exchange: ccmsg.Delivery{Status: "delivered"}}, "iste", true, false, "delivered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := ""
			r := messageReceiver{
				identity: func() error { calls += "i"; return tc.step.identity },
				session: func(context.Context) (ccmsg.Session, error) {
					calls += "s"
					return session, tc.step.session
				},
				token: func(socket string) (string, error) {
					calls += "t"
					if socket != session.SocketPath {
						t.Errorf("token for %q", socket)
					}
					return "peer-key", tc.step.token
				},
				exchange: func(_ context.Context, socket, token, from, body string, waitSec int) (ccmsg.Delivery, error) {
					calls += "e"
					if socket != session.SocketPath || token != "peer-key" || from != "swarm:s1" || body != "Step 1" || waitSec != 0 {
						t.Errorf("exchange: %q %q %q %q %d", socket, token, from, body, waitSec)
					}
					return tc.step.exchange, tc.step.exchangeErr
				},
			}
			got := r.receive(context.Background(), relay.MessageRequest{To: "worker", From: "swarm:s1", Body: "Step 1"})
			if calls != tc.calls || got.OK != tc.ok || got.NotSent != tc.notSent || got.Verdict != tc.verdict {
				t.Fatalf("calls %q, response %#v", calls, got)
			}
		})
	}
}
