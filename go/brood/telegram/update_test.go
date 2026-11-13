package telegram

// FEATURE: Long polling and Update parsing of the Telegram client (#280).
// SPEC: GetUpdates sends offset, limit, timeout and the update kinds the
// package parses; NextOffset confirms every received update and never goes
// back, even after Telegram starts its update IDs anew below the offset.
// An Update carries its message or callback query with from, chat and
// chat type; unknown fields and unknown update kinds are ignored, and an
// update whose known fields do not parse keeps its ID so polling moves past
// it. The long-poll wait, in whole seconds, extends the request deadline. A
// full batch of the longest messages fits the response bound; a larger batch
// wraps ErrTooLarge, and a single update always fits.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type pollParams struct {
	Offset         *int64   `json:"offset"`
	Limit          int      `json:"limit"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

func TestGetUpdatesLongPollsFromOffset(t *testing.T) {
	var mu sync.Mutex
	pending := []int64{10, 11, 12}
	var seen []pollParams
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "getUpdates")
		var p pollParams
		if err := json.Unmarshal(r.body, &p); err != nil {
			t.Errorf("params %q", r.body)
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, p)
		// Как Telegram: offset подтверждает все обновления до него.
		if p.Offset != nil {
			for len(pending) > 0 && pending[0] < *p.Offset {
				pending = pending[1:]
			}
		}
		out := []any{}
		for _, id := range pending[:min(p.Limit, len(pending))] {
			out = append(out, map[string]any{"update_id": id, "message": message(id, "m")})
		}
		result(w, out)
	})
	ctx := context.Background()
	var offset int64
	var got [][]int64
	for range 3 {
		updates, err := c.GetUpdates(ctx, Poll{Offset: offset, Limit: 2, Wait: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, u := range updates {
			ids = append(ids, u.UpdateID)
		}
		got = append(got, ids)
		offset = NextOffset(offset, updates)
	}
	if !reflect.DeepEqual(got, [][]int64{{10, 11}, {12}, nil}) || offset != 13 {
		t.Fatalf("polled %v, offset %d", got, offset)
	}
	kinds := []string{"message", "callback_query"}
	twelve, thirteen := int64(12), int64(13)
	want := []pollParams{{nil, 2, 2, kinds}, {&twelve, 2, 2, kinds}, {&thirteen, 2, 2, kinds}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("poll params %+v, want %+v", seen, want)
	}
}

func TestGetUpdatesDefaultsToAFullBatchOfShortPolls(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
		sameJSON(t, r.body, `{"limit":100,"timeout":0,"allowed_updates":["message","callback_query"]}`)
		result(w, []any{})
	})
	if updates, err := c.GetUpdates(context.Background(), Poll{}); err != nil || len(updates) != 0 || hits.Load() != 1 {
		t.Fatalf("GetUpdates = %v, %v after %d requests", updates, err, hits.Load())
	}
}

func TestGetUpdatesRefusesInvalidPoll(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, []any{}) })
	for name, p := range map[string]Poll{
		"negative offset":   {Offset: -1},
		"negative limit":    {Limit: -1},
		"limit over 100":    {Limit: MaxUpdates + 1},
		"negative wait":     {Wait: -time.Second},
		"wait over the cap": {Wait: MaxPollWait + time.Second},
		// Bot API ждёт целые секунды: дробь ушла бы округлённой вниз, а срок
		// вызова продлился бы на неё целиком.
		"wait of a second and a half": {Wait: 1500 * time.Millisecond},
		"wait of a nanosecond":        {Wait: time.Nanosecond},
		"wait just under the cap":     {Wait: MaxPollWait - time.Millisecond},
	} {
		_, err := c.GetUpdates(context.Background(), p)
		localRefusal(t, name, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid polls reached the server %d times", hits.Load())
	}
}

func TestSecondPollerIsAConflict(t *testing.T) {
	c, _ := fakeBot(t, replying(409, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"}`))
	updates, err := c.GetUpdates(context.Background(), Poll{Offset: 12, Wait: time.Second})
	var e *Error
	if !errors.Is(err, ErrConflict) || errors.Is(err, ErrUncertain) || !errors.As(err, &e) || e.Method != "getUpdates" || updates != nil {
		t.Fatalf("second poller: %v, %v", updates, err)
	}
}

func TestLongPollWaitExtendsTheRequestDeadline(t *testing.T) {
	base, hits := serve(t, func(w http.ResponseWriter, r request) {
		time.Sleep(900 * time.Millisecond)
		result(w, []any{map[string]any{"update_id": 1}})
	})
	c := newClient(t, Config{BaseURL: base, Timeout: 300 * time.Millisecond})
	updates, err := c.GetUpdates(context.Background(), Poll{Wait: time.Second})
	if err != nil || len(updates) != 1 || hits.Load() != 1 {
		t.Fatalf("long poll cut by the request timeout: %v, %v", updates, err)
	}
}

func TestNextOffset(t *testing.T) {
	ids := func(n ...int64) []Update {
		out := make([]Update, len(n))
		for i, id := range n {
			out[i] = Update{UpdateID: id}
		}
		return out
	}
	for _, tc := range []struct {
		current int64
		updates []Update
		want    int64
	}{
		{0, nil, 0},
		{5, nil, 5},
		{0, ids(10, 11), 12},
		{0, ids(12, 10), 13},
		{20, ids(10), 20},
		// Telegram начал ID заново ниже offset: offset не меняется, и
		// вызывающий идёт дальше от NextOffset(0, updates).
		{700_000_000, ids(300_000_000, 300_000_001), 700_000_000},
		{0, ids(300_000_000, 300_000_001), 300_000_002},
	} {
		if got := NextOffset(tc.current, tc.updates); got != tc.want {
			t.Errorf("NextOffset(%d, %v) = %d, want %d", tc.current, tc.updates, got, tc.want)
		}
	}
}

const updatesJSON = `[
 {"update_id":10,"message":{"message_id":5,"from":{"id":7,"is_bot":false,"first_name":"Anton","username":"ermak","language_code":"ru"},"chat":{"id":7,"type":"private","first_name":"Anton","username":"ermak"},"date":1763000000,"text":"/start","entities":[{"type":"bot_command","offset":0,"length":6}]}},
 {"update_id":11,"callback_query":{"id":"cb-1","from":{"id":7,"is_bot":false,"first_name":"Anton","username":"ermak"},"message":{"message_id":6,"from":{"id":42,"is_bot":true,"first_name":"Brood","username":"brood_bot"},"chat":{"id":7,"type":"private"},"date":1763000001,"text":"Approve?"},"chat_instance":"-58210","data":"r:1:yes"}},
 {"update_id":12,"edited_message":{"message_id":5,"chat":{"id":7,"type":"private"},"date":1763000002,"edit_date":1763000003,"text":"/stop"},"future_field":{"x":[1,2]}},
 {"update_id":13,"message":{"message_id":"not-a-number","chat":{"id":7,"type":"private"},"date":1}},
 {"update_id":14,"message":{"message_id":8,"from":{"id":9,"is_bot":false,"first_name":"Guest"},"chat":{"id":-100500,"type":"supergroup","title":"Team"},"date":1763000004,"text":"hi","reply_to_message":{"message_id":6,"chat":{"id":-100500,"type":"supergroup"},"date":1763000001,"text":"Approve?"}}}
]`

func TestGetUpdatesParsesMessagesAndCallbacks(t *testing.T) {
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, json.RawMessage(updatesJSON)) })
	updates, err := c.GetUpdates(context.Background(), Poll{})
	if err != nil {
		t.Fatal(err)
	}
	anton := User{ID: 7, FirstName: "Anton", Username: "ermak"}
	private := Chat{ID: 7, Type: ChatPrivate}
	want := []Update{
		{UpdateID: 10, Message: &Message{MessageID: 5, From: &anton, Chat: Chat{ID: 7, Type: ChatPrivate, Username: "ermak"}, Date: 1763000000, Text: "/start"}},
		{UpdateID: 11, CallbackQuery: &CallbackQuery{ID: "cb-1", From: anton, Data: "r:1:yes", Message: &Message{MessageID: 6, From: &User{ID: 42, IsBot: true, FirstName: "Brood", Username: "brood_bot"}, Chat: private, Date: 1763000001, Text: "Approve?"}}},
		{UpdateID: 12},
		{UpdateID: 13, Malformed: true},
		{UpdateID: 14, Message: &Message{MessageID: 8, From: &User{ID: 9, FirstName: "Guest"}, Chat: Chat{ID: -100500, Type: ChatSupergroup}, Date: 1763000004, Text: "hi", ReplyToMessage: &Message{MessageID: 6, Chat: Chat{ID: -100500, Type: ChatSupergroup}, Date: 1763000001, Text: "Approve?"}}},
	}
	if !reflect.DeepEqual(updates, want) {
		got, _ := json.Marshal(updates)
		exp, _ := json.Marshal(want)
		t.Fatalf("updates\n%s\nwant\n%s", got, exp)
	}
	if !updates[0].Message.Chat.Private() || updates[4].Message.Chat.Private() {
		t.Fatal("chat type misread")
	}
	if NextOffset(0, updates) != 15 {
		t.Fatal("a malformed update stops polling")
	}
}

// escapedText is n units of text as the Bot API writes it: a character
// outside ASCII as a JSON escape of six bytes per UTF-16 unit.
func escapedText(n int) string {
	return strings.Repeat(fmt.Sprintf(`\u%04x`, 'я'), n)
}

// longestReply is an update of a group message at MessageLimit in reply to
// another, with the user and chat fields Telegram sends at their longest.
func longestReply(id int64) string {
	name := escapedText(64)
	user := `{"id":1234567890,"is_bot":false,"first_name":"` + name + `","last_name":"` + name + `","username":"` + strings.Repeat("u", 32) + `","language_code":"ru","is_premium":true}`
	chat := `{"id":-1001234567890,"title":"` + escapedText(128) + `","username":"` + strings.Repeat("g", 32) + `","type":"supergroup","is_forum":true}`
	text := escapedText(MessageLimit)
	msg := func(mid int64, tail string) string {
		return fmt.Sprintf(`{"message_id":%d,"message_thread_id":%d,"from":%s,"chat":%s,"date":1763000000,"edit_date":1763000001,"text":"%s"%s}`, mid, mid, user, chat, text, tail)
	}
	return fmt.Sprintf(`{"update_id":%d,"message":%s}`, id, msg(2*id+1, `,"reply_to_message":`+msg(2*id, "")))
}

// longestBatch is a full batch of longestReply updates from 1.
func longestBatch() string {
	updates := make([]string, MaxUpdates)
	for i := range updates {
		updates[i] = longestReply(int64(i + 1))
	}
	return "[" + strings.Join(updates, ",") + "]"
}

// A full batch of the longest messages, each in reply to another, is the
// largest answer brood expects from getUpdates, about 5 MB: the bound takes
// it whole, and half the bound would refuse it.
func TestFullBatchOfLongestRepliesFitsTheBound(t *testing.T) {
	batch := longestBatch()
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) { reply(w, 200, `{"ok":true,"result":`+batch+`}`) })
	updates, err := c.GetUpdates(context.Background(), Poll{})
	if err != nil || len(updates) != MaxUpdates {
		t.Fatalf("a full batch of %d bytes: %d updates, %v", len(batch), len(updates), err)
	}
	text := strings.Repeat("я", MessageLimit)
	for _, u := range updates {
		if m := u.Message; m == nil || m.Text != text || m.ReplyToMessage == nil || m.ReplyToMessage.Text != text {
			t.Fatalf("update %d misread", u.UpdateID)
		}
	}
}

// An answer over the bound wraps ErrTooLarge with ErrUncertain: the poller
// asks again from the same offset with Limit 1, and a single update fits.
func TestOversizedBatchIsTooLargeAndOneUpdateFits(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
		var p pollParams
		if err := json.Unmarshal(r.body, &p); err != nil {
			t.Errorf("params %q", r.body)
		}
		if p.Limit == 1 {
			reply(w, 200, `{"ok":true,"result":[`+longestReply(7)+`]}`)
			return
		}
		reply(w, 200, `{"ok":true,"result":[`+longestReply(7)+strings.Repeat(" ", maxBody)+`]}`)
	})
	ctx := context.Background()
	updates, err := c.GetUpdates(ctx, Poll{Offset: 7})
	if !errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotSent) || updates != nil {
		t.Fatalf("oversized batch: %v, %v", updates, err)
	}
	updates, err = c.GetUpdates(ctx, Poll{Offset: 7, Limit: 1})
	if err != nil || len(updates) != 1 || updates[0].UpdateID != 7 || hits.Load() != 2 {
		t.Fatalf("one update: %v, %v after %d requests", updates, err, hits.Load())
	}
}

func TestGetUpdatesWithoutUpdateIDIsUncertain(t *testing.T) {
	for _, bad := range []string{`[{"message":{"message_id":1,"chat":{"id":7,"type":"private"},"date":1}}]`, `[{"update_id":"10"}]`, `[{"update_id":-1}]`, `"updates"`, `{}`} {
		c, _ := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, json.RawMessage(bad)) })
		updates, err := c.GetUpdates(context.Background(), Poll{})
		if !errors.Is(err, ErrUncertain) || updates != nil {
			t.Errorf("result %s: %v, %v", bad, updates, err)
		}
	}
}
