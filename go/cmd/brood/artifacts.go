package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// artifactCommandNames — закрытый список артефактов бутстрапа (#155).
var artifactCommandNames = []string{"bro", "brood-guest", "claude"}

// artifactMaxSize — предел одного артефакта, как у маршрута доставки (#155).
const artifactMaxSize = 512 << 20

// artifactHeaderMax — предел поля заголовка (имя или размер) до NUL.
const artifactHeaderMax = 64

// cmdArtifacts публикует артефакты бутстрапа из stdin в каталог (--dir):
// записи «имя NUL размер NUL байты» для каждого имени закрытого списка;
// байты — ровно размер и могут содержать NUL, как любой ELF. Поток пишется
// сразу в staging-каталог рядом с dir, затем rename заменяет dir целиком:
// частичная публикация невидима серверу, который перечитывает манифест на
// каждый запрос, а отказ оставляет прежний каталог. Образ сервера — scratch
// без shell и tar, поэтому поток разбирает сам бинарь (#156); вызывает его
// выкладка через kubectl exec -i.
func cmdArtifacts(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("artifacts", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "artifact directory to publish")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "brood artifacts: invalid arguments; use --dir")
		return 1
	}
	if *dir == "" || !filepath.IsAbs(*dir) || filepath.Base(*dir) == "" || filepath.Base(*dir) == "." {
		fmt.Fprintln(stderr, "brood artifacts: --dir must be an absolute directory path")
		return 1
	}
	manifest, err := publishArtifacts(*dir, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "brood artifacts: %s\n", err)
		return 1
	}
	// manifest уже собран как JSON-байты: Encode([]byte) дал бы base64,
	// поэтому пишем сами байты.
	if _, err := stdout.Write(append(manifest, '\n')); err != nil {
		fmt.Fprintln(stderr, "brood artifacts: cannot write the manifest")
		return 1
	}
	return 0
}

// publishArtifacts принимает поток в staging и заменяет им dir. Любой отказ
// удаляет staging и оставляет прежний dir на месте.
func publishArtifacts(dir string, stream io.Reader) ([]byte, error) {
	suffix := strconv.Itoa(os.Getpid())
	staging, old := dir+".tmp-"+suffix, dir+".old-"+suffix
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("staging directory is unavailable")
	}
	if err := os.MkdirAll(staging, 0755); err != nil {
		return nil, fmt.Errorf("staging directory is unavailable")
	}
	defer os.RemoveAll(staging)
	entries, err := receiveArtifacts(staging, bufio.NewReaderSize(stream, 1<<20))
	if err != nil {
		return nil, err
	}
	manifest, err := json.Marshal(map[string]any{"artifacts": entries})
	if err != nil {
		return nil, fmt.Errorf("manifest encoding failed")
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), manifest, 0644); err != nil {
		return nil, fmt.Errorf("staging directory is unavailable")
	}
	if err := os.Rename(dir, old); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("publish is unavailable")
	}
	if err := os.Rename(staging, dir); err != nil {
		// Прежний каталог возвращается на место: сервер продолжает отдавать его.
		os.Rename(old, dir)
		return nil, fmt.Errorf("publish is unavailable")
	}
	os.RemoveAll(old)
	return manifest, nil
}

// receiveArtifacts пишет записи потока в файлы staging, считая sha256 на
// лету; каждое имя закрытого списка ровно один раз, байты — с ELF-магией.
// Записи возвращаются в порядке закрытого списка.
func receiveArtifacts(staging string, r *bufio.Reader) ([]map[string]any, error) {
	invalid := fmt.Errorf("artifact stream is invalid")
	received := map[string]map[string]any{}
	for {
		name, err := readArtifactHeader(r)
		if err == io.EOF {
			break
		}
		if err != nil || !knownArtifact(name) || received[name] != nil {
			return nil, invalid
		}
		field, err := readArtifactHeader(r)
		size, convErr := strconv.ParseInt(field, 10, 64)
		if err != nil || convErr != nil || size <= 0 || size > artifactMaxSize || strconv.FormatInt(size, 10) != field {
			return nil, invalid
		}
		digest, err := receiveArtifact(filepath.Join(staging, name), r, size)
		if err != nil {
			return nil, invalid
		}
		received[name] = map[string]any{"name": name, "size": size, "sha256": digest}
	}
	entries := []map[string]any{}
	for _, name := range artifactCommandNames {
		if received[name] == nil {
			return nil, fmt.Errorf("artifact stream is incomplete")
		}
		entries = append(entries, received[name])
	}
	return entries, nil
}

// readArtifactHeader читает короткое поле до NUL; io.EOF — только на границе
// записи, обрыв внутри поля — ошибка.
func readArtifactHeader(r *bufio.Reader) (string, error) {
	field := make([]byte, 0, artifactHeaderMax)
	for len(field) <= artifactHeaderMax {
		b, err := r.ReadByte()
		if err == io.EOF && len(field) == 0 {
			return "", io.EOF
		}
		if err != nil {
			return "", fmt.Errorf("truncated header")
		}
		if b == 0 {
			return string(field), nil
		}
		field = append(field, b)
	}
	return "", fmt.Errorf("oversized header")
}

// receiveArtifact копирует ровно size байт в файл 0755 и возвращает sha256;
// первые четыре байта обязаны быть ELF-магией.
func receiveArtifact(path string, r *bufio.Reader, size int64) (string, error) {
	magic, err := r.Peek(4)
	if err != nil || string(magic) != "\x7fELF" {
		return "", fmt.Errorf("not an ELF binary")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(file, hash), r, size); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func knownArtifact(name string) bool {
	for _, allowed := range artifactCommandNames {
		if name == allowed {
			return true
		}
	}
	return false
}
