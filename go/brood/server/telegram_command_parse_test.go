package server

// FEATURE: Parsing and paging of Telegram commands (#284).
// SPEC: a command splits into its words, and the text after the words a
// command takes keeps its line breaks: the goal of /start goes to the store
// as typed, trimmed at its ends. A page of a list holds telegramPageSize
// items, each clipped to telegramLineLimit characters, names its number
// and the number of pages, and points to the next page; a page past the
// end and a page that is not a number are refused with their reason.
// IMPLEMENTATION: telegramFields, telegramPaged (telegram_commands.go).
// STATUS: DONE

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"brood.dev/go/brood/telegram"
)

func TestTelegramFieldsKeepTheRestAsTyped(t *testing.T) {
	for _, tc := range []struct {
		text   string
		n      int
		fields []string
		rest   string
	}{
		{"/start alpha Ship it", 2, []string{"/start", "alpha"}, "Ship it"},
		{"/start   alpha\n Ship\nit  \n", 2, []string{"/start", "alpha"}, "Ship\nit"},
		{"/start alpha", 2, []string{"/start", "alpha"}, ""},
		{"/start", 2, []string{"/start"}, ""},
		{"/actor alpha coding stop extra", 4, []string{"/actor", "alpha", "coding", "stop"}, "extra"},
		{"", 1, nil, ""},
		{"/x\ta b c", 2, []string{"/x", "a"}, "b c"},
	} {
		fields, rest := telegramFields(tc.text, tc.n)
		if !slices.Equal(fields, tc.fields) || rest != tc.rest {
			t.Errorf("telegramFields(%q, %d) = %q, %q; want %q, %q", tc.text, tc.n, fields, rest, tc.fields, tc.rest)
		}
	}
}

func TestTelegramPagedListNamesItsPageAndTheNext(t *testing.T) {
	items := make([]string, 2*telegramPageSize+3)
	for i := range items {
		items[i] = fmt.Sprintf("item %d", i)
	}
	first, refusal := telegramPaged("Things", items, "", "/things", 1)
	if refusal != "" {
		t.Fatal(refusal)
	}
	want := "Things, page 1 of 3:\n- " + strings.Join(items[:telegramPageSize], "\n- ") + "\nNext page: /things 2"
	if first != want {
		t.Fatalf("first page:\n%s\nwant:\n%s", first, want)
	}
	last, _ := telegramPaged("Things", items, "Footer.", "/things", 3)
	if want := "Things, page 3 of 3:\n- " + strings.Join(items[2*telegramPageSize:], "\n- ") + "\nFooter."; last != want {
		t.Fatalf("last page:\n%s\nwant:\n%s", last, want)
	}
	if _, refusal := telegramPaged("Things", items, "", "/things", 4); refusal != "There is no page 4: the list has 3 pages." {
		t.Fatalf("page past the end: %q", refusal)
	}
	if _, refusal := telegramPaged("Things", items[:1], "", "/things", 2); refusal != "There is no page 2: the list has 1 page." {
		t.Fatalf("page past the end of one page: %q", refusal)
	}
	// Длинные строки обрезаются, страница остаётся в пределе сообщения.
	long := make([]string, telegramPageSize)
	for i := range long {
		long[i] = strings.Repeat("𝔹", 5000)
	}
	page, _ := telegramPaged(strings.Repeat("T", 5000), long, strings.Repeat("F", 5000), "/things", 1)
	if telegram.TextLength(page) > telegram.MessageLimit {
		t.Fatalf("page of long lines: %d characters", telegram.TextLength(page))
	}
	for _, line := range strings.Split(page, "\n") {
		if telegram.TextLength(line) > telegramLineLimit {
			t.Fatalf("line of %d characters", telegram.TextLength(line))
		}
	}
}

func TestTelegramPageArgument(t *testing.T) {
	for arg, want := range map[string]int{"": 1, "1": 1, "7": 7} {
		if got, ok := telegramPageNumber(arg); !ok || got != want {
			t.Errorf("page %q = %d %v, want %d", arg, got, ok, want)
		}
	}
	for _, arg := range []string{"0", "-1", "two", "1.5", "99999999999999999999"} {
		if _, ok := telegramPageNumber(arg); ok {
			t.Errorf("page %q accepted", arg)
		}
	}
}
