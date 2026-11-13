package server

// FEATURE: Installation Telegram bot settings (#281).
// SPEC: BROOD_TELEGRAM_BOT_TOKEN is an optional installation bot token, a
// secret of .env: a malformed one is refused naming the setting, never the
// value, and no formatting of Config prints it. BROOD_TELEGRAM_API is the Bot
// API origin, the public https://api.telegram.org by default, HTTPS or HTTP on
// loopback, refused naming the setting otherwise.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"brood.dev/go/brood/telegram"
)

func TestTelegramSettingsLoad(t *testing.T) {
	values := map[string]string{
		"BROOD_TELEGRAM_BOT_TOKEN": "'" + installBotToken + "'",
		"BROOD_TELEGRAM_API":       "http://127.0.0.1:8081",
	}
	cfg, err := LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.telegramBotToken != installBotToken || cfg.TelegramAPI != "http://127.0.0.1:8081" {
		t.Fatalf("telegram settings not loaded: api %q", cfg.TelegramAPI)
	}
}

func TestTelegramSettingsDefaultToThePublicBotAPIWithoutABot(t *testing.T) {
	cfg, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramAPI != telegram.DefaultBaseURL || telegram.DefaultBaseURL != "https://api.telegram.org" || cfg.telegramBotToken != "" {
		t.Fatalf("telegram defaults: api %q, bot set %v", cfg.TelegramAPI, cfg.telegramBotToken != "")
	}
}

func TestTelegramSettingsRefuseMalformedValuesWithoutPrintingThem(t *testing.T) {
	for _, tc := range []struct{ setting, value string }{
		{"BROOD_TELEGRAM_BOT_TOKEN", "SENTINEL-not-a-bot-token"},
		{"BROOD_TELEGRAM_BOT_TOKEN", "123:SENTINEL secret with spaces"},
		{"BROOD_TELEGRAM_API", "http://SENTINEL.example:8081"},
		{"BROOD_TELEGRAM_API", "https://api.telegram.org/SENTINEL"},
	} {
		_, err := LoadConfig(func(key string) string {
			if key == tc.setting {
				return tc.value
			}
			return ""
		})
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) || cfgErr.Setting != tc.setting {
			t.Errorf("%s=%q: error %v, want a ConfigError naming the setting", tc.setting, tc.value, err)
			continue
		}
		if strings.Contains(err.Error(), "SENTINEL") {
			t.Errorf("%s refusal prints the value: %v", tc.setting, err)
		}
	}
}

// Сборка сервера из Config, собранного в коде, проверяет те же правила.
func TestTelegramSettingsAreCheckedWhenTheServerIsBuilt(t *testing.T) {
	cfg := Config{DBPath: t.TempDir() + "/brood.db", telegramBotToken: "SENTINEL-not-a-bot-token"}
	srv, err := New(nil, cfg, Options{Provisioner: newFakeProvisioner()})
	if err == nil {
		srv.Close()
		t.Fatal("a malformed installation bot token was accepted")
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("refusal prints the token: %v", err)
	}
}

func TestTelegramBotTokenStaysOutOfFormattedConfig(t *testing.T) {
	cfg := Config{TelegramAPI: telegram.DefaultBaseURL, telegramBotToken: installBotToken}
	for _, out := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprint(&cfg)} {
		if strings.Contains(out, installSecret) {
			t.Fatalf("formatted config carries the bot token: %s", out)
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), installSecret) {
		t.Fatalf("marshaled config carries the bot token: %s", data)
	}
}
