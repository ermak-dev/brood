package server

// FEATURE: Pure helpers of the Telegram pollers (#281).
// SPEC: the backoff starts at a second, doubles per failure and stops at a
// minute; a bot command is "/name", "/name@bot" and its argument; a link
// error reason is clipped on a character boundary; the bot id is the part of
// a BotFather token before the colon. A link code is 32 bytes of its source in
// URL-safe base64, the source is crypto/rand, and a short read refuses the
// code. Stopping a poller returns only after its loop ended.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/telegram"
)

func TestTelegramBackoffDoublesUpToAMinute(t *testing.T) {
	for failures, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 6: 32 * time.Second, 7: time.Minute, 50: time.Minute} {
		if got := telegramBackoff(failures); got != want {
			t.Errorf("telegramBackoff(%d) = %s, want %s", failures, got, want)
		}
	}
}

func TestTelegramCommandParsesNameAddresseeAndArgument(t *testing.T) {
	for _, tc := range []struct {
		text, name, arg string
		ok              bool
	}{
		{"/start abc", "start", "abc", true},
		{"  /start   abc  ", "start", "abc", true},
		{"/start@Alice_Bot abc", "start", "abc", true},
		{"/start", "start", "", true},
		{"/help@Alice_Bot", "help", "", true},
		{"/start a b", "start", "a b", true},
		{"/start\nabc", "start", "abc", true},
		{"hello /start abc", "", "", false},
		{"/", "", "", false},
		{"", "", "", false},
	} {
		name, arg, ok := telegramCommand(tc.text)
		if name != tc.name || arg != tc.arg || ok != tc.ok {
			t.Errorf("telegramCommand(%q) = %q, %q, %v; want %q, %q, %v", tc.text, name, arg, ok, tc.name, tc.arg, tc.ok)
		}
	}
}

func TestTelegramClipKeepsWholeCharacters(t *testing.T) {
	if got := telegramClip("short", 10); got != "short" {
		t.Fatalf("clip of a short text = %q", got)
	}
	got := telegramClip("abcdéf", 5)
	if got != "abcd" || !utf8.ValidString(got) {
		t.Fatalf("clip inside a character = %q", got)
	}
}

func TestTelegramLinkCodeIsItsSourceInBase64URL(t *testing.T) {
	source := make([]byte, telegramLinkCodeBytes)
	for i := range source {
		source[i] = byte(251 - 5*i)
	}
	code, err := newTelegramLinkCode(bytes.NewReader(source))
	if err != nil || code != base64.RawURLEncoding.EncodeToString(source) {
		t.Fatalf("link code of known bytes = %q, %v", code, err)
	}
	if code, err := newTelegramLinkCode(bytes.NewReader(source[:telegramLinkCodeBytes-1])); err == nil {
		t.Fatalf("a short source gave the link code %q", code)
	}
}

func TestTelegramLinkCodeSourceIsCryptoRand(t *testing.T) {
	if (telegramOptions{}).withDefaults().random != rand.Reader {
		t.Fatal("the default link code source is not crypto/rand")
	}
	if telegramLinkCodeBytes < 32 {
		t.Fatalf("link code of %d random bytes, want 256 bits", telegramLinkCodeBytes)
	}
}

// stop возвращается, только когда цикл поллера кончился: Server.Close и
// DELETE полагаются на то, что после stop поллер не пишет в базу и не
// зовёт Bot API.
func TestTelegramPollerStopWaitsForItsLoop(t *testing.T) {
	client, err := telegram.NewClient(telegramProbeToken, telegram.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &telegramPoller{client: client, cancel: cancel, done: make(chan struct{})}
	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(p.done)
	}()
	p.stop()
	if p.alive() {
		t.Fatal("stop returned before the poller loop ended")
	}
}

func TestTelegramBotIDIsTheTokenPrefix(t *testing.T) {
	for token, want := range map[string]int64{installBotToken: 710000009, "42:secret": 42, "": 0, "secret": 0, "x1:secret": 0} {
		if got := telegramBotID(token); got != want {
			t.Errorf("telegramBotID(%q) = %d, want %d", token, got, want)
		}
	}
}
