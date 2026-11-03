package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FEATURE: Деплой кладёт артефакты бутстрапа в PVC сервера (#156).
// SPEC: brood artifacts --dir DIR читает из stdin поток length-prefixed
// бинарников и пишет их в DIR с manifest.json формата #155 (имена bro,
// brood-guest, claude, отсортированы, sha256+size). Атомарно: сначала во
// временный каталог DIR.tmp-<pid>, затем rename; существующий каталог
// заменяется целиком. Отказ — одна строка причины, без путей и тел.
func TestArtifactsCommandPublishesVerifiedManifest(t *testing.T) {
	dir := t.TempDir()
	payloads := map[string]string{
		"bro":         "\x7fELFbro-bin",
		"brood-guest": "\x7fELFguest-bin",
		"claude":      "\x7fELFclaude-bin",
	}
	var stream bytes.Buffer
	for _, name := range []string{"bro", "brood-guest", "claude"} {
		stream.WriteString(name)
		stream.WriteByte(0)
		stream.WriteString(payloads[name])
		stream.WriteByte(0)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"artifacts", "--dir", dir}, &stream, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("artifacts failed: %d %s", code, stderr.String())
	}
	var manifest struct {
		Artifacts []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil || len(manifest.Artifacts) != 3 {
		t.Fatalf("manifest output: %s %v", stdout.String(), err)
	}
	wantNames := []string{"bro", "brood-guest", "claude"}
	for i, entry := range manifest.Artifacts {
		if entry.Name != wantNames[i] {
			t.Fatalf("entry %d name %q, want %q", i, entry.Name, wantNames[i])
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name))
		if err != nil || string(data) != payloads[entry.Name] {
			t.Fatalf("%s content: %q %v", entry.Name, data, err)
		}
		sum := sha256.Sum256([]byte(payloads[entry.Name]))
		if entry.SHA256 != hex.EncodeToString(sum[:]) || entry.Size != int64(len(payloads[entry.Name])) {
			t.Fatalf("%s digest/size: %+v", entry.Name, entry)
		}
	}
	// Повторная публикация заменяет каталог целиком: старых файлов нет.
	if err := os.WriteFile(filepath.Join(dir, "stale"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	stream.Reset()
	for _, name := range wantNames {
		stream.WriteString(name + "\x00" + payloads[name] + "\x00")
	}
	stdout.Reset()
	if code := run([]string{"artifacts", "--dir", dir}, &stream, &stdout, &stderr); code != 0 {
		t.Fatalf("republish failed: %d %s", code, stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "stale")); !os.IsNotExist(err) {
		t.Fatal("stale file survived republish")
	}
}

// Отказы: пустой поток, неизвестное имя, обрыв записи, дубликат, non-ELF.
func TestArtifactsCommandRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{"empty", ""},
		{"unknown name", "wget\x00\x7fELFdata\x00"},
		{"duplicate", "bro\x00\x7fELFa\x00bro\x00\x7fELFb\x00"},
		{"missing claude", "bro\x00\x7fELFa\x00brood-guest\x00\x7fELFb\x00"},
		{"truncated", "bro\x00\x7fELFa"},
		{"not elf", "bro\x00text\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"artifacts", "--dir", t.TempDir()}, strings.NewReader(tc.stream), &stdout, &stderr)
			if code == 0 || stdout.Len() != 0 || stderr.String() == "" || strings.Count(stderr.String(), "\n") != 1 {
				t.Fatalf("refusal %q: code=%d out=%q err=%q", tc.name, code, stdout.String(), stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"artifacts"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("missing --dir accepted")
	}
}
