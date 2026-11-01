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
	if !reflect.DeepEqual(s.Env, actorEnv()) {
		t.Fatal("helper environment differs from actor")
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
	if _, err := ReceiveMessage(context.Background(), relay.MessageRequest{To: "worker", From: "lead", Body: "hello"}); err == nil || err.Error() != "message helper requires UID and GID 1000" {
		t.Fatalf("privilege gate: %v", err)
	}
}

func TestDeliverMessageRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root supervisor only")
	}
	got := DeliverMessage(context.Background(), relay.MessageRequest{To: "a", From: "b", Body: "c"})
	if got.OK || got.Verdict != "message delivery requires guest supervisor" {
		t.Fatalf("root gate: %#v", got)
	}
}
