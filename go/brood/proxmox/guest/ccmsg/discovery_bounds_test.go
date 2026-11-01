package ccmsg

// FEATURE: Ограниченное чтение actor-owned файлов сессий и ключей.
// SPEC: Файл больше 128 KiB не разбирается даже при валидном JSON.
// IMPLEMENTATION: Ограничение до и во время чтения session/key JSON.
// RESULT: IMPLEMENTED — oversized files отвергаются.
// STATUS: DONE

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionDiscoveryRejectsOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	large := `{"pid":42,"messagingSocketPath":"/tmp/42.sock","cwd":"/project"}` + strings.Repeat(" ", 128*1024)
	if err := os.WriteFile(filepath.Join(dir, "42.json"), []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	sessions, err := FindSessions(dir)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("oversized session accepted: %#v %v", sessions, err)
	}
	path := filepath.Join(dir, "42.sock")
	key := filepath.Join(dir, "42."+sha256Hex(path)+".key")
	if err := os.WriteFile(key, []byte(`{"peerToken":"secret"}`+strings.Repeat(" ", 128*1024)), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := PeerToken(path, dir); err == nil {
		t.Fatalf("oversized key accepted: %s", token)
	}
}
