package telegram

// FEATURE: Message text limits of the Bot API (#280).
// SPEC: TextLength counts UTF-16 code units, an upper bound of the
// characters Telegram counts. SplitText cuts a long text into messages of at
// most MessageLimit units, preferring a line break and then a space in the
// second half of a message, never inside a code point; the pieces joined
// give the text back, and a piece of whitespace only, which Telegram would
// refuse as empty, is dropped.

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// astral is U+1F600, a code point outside the Basic Multilingual Plane: four
// UTF-8 bytes, two UTF-16 units.
const astral = "\U0001F600"

func TestTextLengthCountsUTF16Units(t *testing.T) {
	if len(astral) != 4 || utf8.RuneCountInString(astral) != 1 {
		t.Fatalf("astral is %d bytes of %d code points", len(astral), utf8.RuneCountInString(astral))
	}
	for s, want := range map[string]int{"": 0, "abc": 3, "привет": 6, astral: 2, "a" + astral + "b": 4, "\xff": 1} {
		if got := TextLength(s); got != want {
			t.Errorf("TextLength(%q) = %d, want %d", s, got, want)
		}
	}
}

// requireMessages checks that every piece fits one message, is valid UTF-8,
// is not blank, and that the pieces give the text back.
func requireMessages(t *testing.T, name, text string, pieces []string) {
	t.Helper()
	for i, p := range pieces {
		if TextLength(p) > MessageLimit || strings.TrimSpace(p) == "" || !utf8.ValidString(p) {
			t.Fatalf("%s: piece %d of %d units is not a message", name, i, TextLength(p))
		}
	}
	if strings.Join(pieces, "") != text {
		t.Fatalf("%s: pieces do not give the text back", name)
	}
}

func TestSplitText(t *testing.T) {
	a, b := strings.Repeat("a", 3000), strings.Repeat("b", 3000)
	words := strings.Repeat("word ", 3000)
	pairs := "a" + strings.Repeat(astral, MessageLimit/2)
	cases := []struct {
		name, text string
		want       []string
	}{
		{"short", "Swarm started.", []string{"Swarm started."}},
		{"exactly the limit", strings.Repeat("a", MessageLimit), []string{strings.Repeat("a", MessageLimit)}},
		{"one over the limit", strings.Repeat("a", MessageLimit+1), []string{strings.Repeat("a", MessageLimit), "a"}},
		{"line break", a + "\n" + b, []string{a + "\n", b}},
		{"line break over a space", a[:2000] + " " + a[:1500] + "\n" + b, []string{a[:2000] + " " + a[:1500] + "\n", b}},
		{"early line break is not taken", "a\n" + b + b, []string{"a\n" + (b + b)[:MessageLimit-2], (b + b)[MessageLimit-2:]}},
		{"space", a + " " + b, []string{a + " ", b}},
		{"surrogate pair at the cut", pairs, []string{pairs[:1+4*(MessageLimit/2-1)], astral}},
	}
	for _, tc := range cases {
		got := SplitText(tc.text)
		requireMessages(t, tc.name, tc.text, got)
		if tc.want != nil && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %d pieces of %v units", tc.name, len(got), lengths(got))
		}
	}
	got := SplitText(words)
	requireMessages(t, "words", words, got)
	for _, p := range got[:len(got)-1] {
		if !strings.HasSuffix(p, " ") {
			t.Fatal("words: a piece ends inside a word")
		}
	}
}

func lengths(pieces []string) []int {
	out := make([]int, len(pieces))
	for i, p := range pieces {
		out[i] = TextLength(p)
	}
	return out
}

func TestSplitTextDropsBlankPieces(t *testing.T) {
	for _, s := range []string{"", " ", "\n\n", strings.Repeat(" ", 3*MessageLimit)} {
		if got := SplitText(s); got != nil {
			t.Errorf("SplitText of %d blank units = %d pieces", len(s), len(got))
		}
	}
	text := "start" + strings.Repeat(" ", 2*MessageLimit) + "end"
	got := SplitText(text)
	if len(got) != 2 || !strings.HasPrefix(got[0], "start") || !strings.HasSuffix(got[1], "end") {
		t.Fatalf("blank run: %d pieces of %v units", len(got), lengths(got))
	}
}
