package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// FEATURE: Деплой кладёт артефакты бутстрапа в PVC сервера (#156).
// SPEC: brood artifacts --dir DIR читает из stdin записи «имя NUL размер NUL
// байты» (байты — ровно размер, могут содержать NUL, как любой ELF) и пишет
// их в DIR с manifest.json формата #155 (имена bro, brood-guest, claude,
// отсортированы, sha256+size). Атомарно: staging-каталог, затем rename;
// отказ оставляет прежний опубликованный каталог нетронутым. Отказ — одна
// строка причины, без путей и тел.
// BUG: формат «имя NUL байты NUL» резал настоящий ELF на первом нуле (прод 6757).

// elfPayload — бинарь с нулевыми байтами, как настоящий ELF-заголовок.
func elfPayload(tag string) string {
	return "\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00" + tag + "\x00\x00tail"
}

func artifactFrame(name, data string) string {
	return name + "\x00" + strconv.Itoa(len(data)) + "\x00" + data
}

func artifactStream(payloads map[string]string) *bytes.Buffer {
	var stream bytes.Buffer
	for _, name := range []string{"bro", "brood-guest", "claude"} {
		stream.WriteString(artifactFrame(name, payloads[name]))
	}
	return &stream
}

func TestArtifactsCommandPublishesVerifiedManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	payloads := map[string]string{
		"bro":         elfPayload("bro"),
		"brood-guest": elfPayload("guest"),
		"claude":      elfPayload("claude"),
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"artifacts", "--dir", dir}, artifactStream(payloads), &stdout, &stderr)
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
	written, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil || !bytes.Equal(bytes.TrimSpace(stdout.Bytes()), written) {
		t.Fatalf("published manifest differs from output: %q %v", written, err)
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
	stdout.Reset()
	if code := run([]string{"artifacts", "--dir", dir}, artifactStream(payloads), &stdout, &stderr); code != 0 {
		t.Fatalf("republish failed: %d %s", code, stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "stale")); !os.IsNotExist(err) {
		t.Fatal("stale file survived republish")
	}
	if leftovers, _ := filepath.Glob(dir + ".*"); len(leftovers) != 0 {
		t.Fatalf("staging leftovers: %v", leftovers)
	}
}

// Отказы не трогают опубликованный каталог и не оставляют staging.
func TestArtifactsCommandRefuses(t *testing.T) {
	good := map[string]string{"bro": elfPayload("a"), "brood-guest": elfPayload("b"), "claude": elfPayload("c")}
	valid := artifactStream(good).String()
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{"empty", ""},
		{"unknown name", artifactFrame("wget", elfPayload("x"))},
		{"duplicate", artifactFrame("bro", elfPayload("a")) + artifactFrame("bro", elfPayload("b"))},
		{"missing claude", artifactFrame("bro", elfPayload("a")) + artifactFrame("brood-guest", elfPayload("b"))},
		{"truncated", valid[:len(valid)-3]},
		{"trailing bytes", valid + "x"},
		{"not elf", artifactFrame("bro", "text") + valid[len(artifactFrame("bro", good["bro"])):]},
		{"zero size", "bro\x000\x00"},
		{"bad size", "bro\x00-5\x00"},
		{"oversized", "bro\x00" + strconv.Itoa(512<<20+1) + "\x00\x7fELF"},
		{"header without NUL", strings.Repeat("b", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "artifacts")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("published"), 0644); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"artifacts", "--dir", dir}, strings.NewReader(tc.stream), &stdout, &stderr)
			if code == 0 || stdout.Len() != 0 || stderr.String() == "" || strings.Count(stderr.String(), "\n") != 1 {
				t.Fatalf("refusal %q: code=%d out=%q err=%q", tc.name, code, stdout.String(), stderr.String())
			}
			if data, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err != nil || string(data) != "published" {
				t.Fatalf("refusal changed published artifacts: %q %v", data, err)
			}
			if leftovers, _ := filepath.Glob(dir + ".*"); len(leftovers) != 0 {
				t.Fatalf("staging leftovers: %v", leftovers)
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"artifacts"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("missing --dir accepted")
	}
}
