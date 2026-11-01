//go:build linux

package main

// FEATURE: Выделенный message-helper до чтения supervisor env.
// SPEC: Точный флаг, запрет аргументов от актора, реальный relay handler.
// IMPLEMENTATION: Dispatch до supervisor env, stdin/stdout JSON helper.
// RESULT: IMPLEMENTED — приём сообщений подключён в конфигурации.
// STATUS: DONE

import (
	"context"
	"testing"
)

func TestMessageHelperDispatchBeforeEnv(t *testing.T) {
	calls := 0
	d := dependencies{message: func(context.Context) error { calls++; return nil }}
	env := func() []string { t.Fatal("message helper read supervisor environment"); return nil }
	if err := dispatch(context.Background(), []string{"--brood-guest-message"}, env, d); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if err := dispatch(context.Background(), []string{"--brood-guest-message", "/root/service.sock"}, env, d); err == nil {
		t.Fatal("actor-selected arguments accepted")
	}
}

func TestRelayHasRealMessageHandler(t *testing.T) {
	cfg, err := parseConfig(validEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.relay.HandleMessage == nil {
		t.Fatal("guest message handler not wired")
	}
}
