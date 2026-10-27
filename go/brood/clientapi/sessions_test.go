package clientapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *SessionStore {
	t.Helper()
	s, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	return s
}

func TestStoreSaveLoadRoundtrip(t *testing.T) {
	s := newTestStore(t)
	sess := StoredSession{Session: "sess-1", CSRF: "csrf-1"}
	if err := s.Save("https://brood.example", sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := s.Load("https://brood.example")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got != sess {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestStoreIsolatesOrigins(t *testing.T) {
	s := newTestStore(t)
	_ = s.Save("https://one.example", StoredSession{Session: "a", CSRF: "a"})
	_ = s.Save("https://two.example", StoredSession{Session: "b", CSRF: "b"})
	got, ok, _ := s.Load("https://two.example")
	if !ok || got.Session != "b" {
		t.Fatalf("second origin overwritten: %+v", got)
	}
	got, ok, _ = s.Load("https://one.example")
	if !ok || got.Session != "a" {
		t.Fatalf("first origin overwritten: %+v", got)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.Load("https://missing.example")
	if ok || err != nil {
		t.Fatalf("missing session reported as present: ok=%v err=%v", ok, err)
	}
}

func TestStoreDelete(t *testing.T) {
	s := newTestStore(t)
	_ = s.Save("https://brood.example", StoredSession{Session: "a", CSRF: "a"})
	if err := s.Delete("https://brood.example"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Load("https://brood.example"); ok {
		t.Fatal("session survived Delete")
	}
	// Повторное удаление отсутствующего файла — успех.
	if err := s.Delete("https://brood.example"); err != nil {
		t.Fatalf("Delete of missing file: %v", err)
	}
}

func TestStorePermissionsAndAtomicity(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	_ = s.Save("https://brood.example", StoredSession{Session: "a", CSRF: "a"})
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %o, want 0700", info.Mode().Perm())
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*"))
		if len(files) != 1 {
			t.Fatalf("unexpected files in store dir: %v", files)
		}
		info, err = os.Stat(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %o, want 0600", info.Mode().Perm())
		}
	}
	// После записи не остаётся временных файлов.
	_ = s.Save("https://brood.example", StoredSession{Session: "b", CSRF: "b"})
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" && e.Name() != "current" {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestStoreCurrentServer(t *testing.T) {
	s := newTestStore(t)
	if cur, err := s.Current(); err != nil || cur != "" {
		t.Fatalf("empty current: %q, %v", cur, err)
	}
	if err := s.SetCurrent("https://brood.example"); err != nil {
		t.Fatalf("SetCurrent: %v", err)
	}
	cur, err := s.Current()
	if err != nil || cur != "https://brood.example" {
		t.Fatalf("current = %q, %v", cur, err)
	}
	if err := s.SetCurrent(""); err != nil {
		t.Fatalf("SetCurrent empty: %v", err)
	}
	if cur, _ := s.Current(); cur != "" {
		t.Fatalf("current not cleared: %q", cur)
	}
}

func TestStoreSessionFileNeverHasPassword(t *testing.T) {
	// Формат StoredSession не несёт пароля; фиксируем намерение тестом
	// на содержимое файла.
	s := newTestStore(t)
	if err := s.Save("https://brood.example", StoredSession{Session: "s", CSRF: "c"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, sessionFileName("https://brood.example")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "password") {
		t.Fatalf("session file mentions password: %s", raw)
	}
}
