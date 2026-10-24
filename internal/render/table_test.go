package render

import "testing"

func TestTable(t *testing.T) {
	got := Table([][]string{{"a", "bb", "текст"}, {"ccc", "d", "x"}})
	want := []string{"a    bb  текст", "ccc  d   x"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Table = %q, want %q", got, want)
	}
	// Последняя колонка не добивается пробелами, но печатается.
	got = Table([][]string{{"k", "value"}})
	if got[0] != "k  value" {
		t.Errorf("last column unpadded: %q", got[0])
	}
	// Ширина по видимой части; короче последней колонки строка не рвёт.
	got = Table([][]string{{"ключ", "x", "хвост"}, {"k", "x", "хвост"}})
	if got[1] != "k         x  хвост" {
		t.Errorf("column width by longest: %q", got[1])
	}
	if Table(nil) != nil {
		t.Error("no rows, no lines")
	}
}
