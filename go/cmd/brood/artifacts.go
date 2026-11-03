package main

import (
	"bytes"
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

// cmdArtifacts публикует артефакты бутстрапа из stdin в каталог (--dir):
// записи «имя NUL байты NUL» для каждого имени закрытого списка. Каталог
// заменяется целиком через временный каталог и rename: частичная публикация
// невидима серверу, который перечитывает манифест на каждый запрос. Образ
// сервера — scratch без shell и tar, поэтому поток разбирает сам бинарь
// (#156); вызывает его выкладка через kubectl exec -i.
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
	payloads, err := readArtifactStream(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "brood artifacts: %s\n", err)
		return 1
	}
	manifest, err := publishArtifacts(*dir, payloads)
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

// readArtifactStream читает записи «имя NUL байты NUL»; порядок любой, каждое
// имя закрытого списка ровно один раз, байты начинаются с ELF-магии.
func readArtifactStream(r io.Reader) (map[string][]byte, error) {
	payloads := map[string][]byte{}
	for {
		name, err := readNulField(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("artifact stream is invalid")
		}
		known := false
		for _, allowed := range artifactCommandNames {
			if name == allowed {
				known = true
			}
		}
		if _, duplicate := payloads[name]; !known || duplicate {
			return nil, fmt.Errorf("artifact stream is invalid")
		}
		data, err := readNulField(r)
		if err != nil || len(data) < 4 || string(data[:4]) != "\x7fELF" {
			return nil, fmt.Errorf("artifact stream is invalid")
		}
		payloads[name] = []byte(data)
	}
	for _, name := range artifactCommandNames {
		if payloads[name] == nil {
			return nil, fmt.Errorf("artifact stream is incomplete")
		}
	}
	return payloads, nil
}

// readNulField читает поле до NUL; лимит отсекает неограниченное чтение.
func readNulField(r io.Reader) (string, error) {
	var field bytes.Buffer
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == 0 {
				if field.Len() > 512<<20 {
					return "", fmt.Errorf("oversized")
				}
				return field.String(), nil
			}
			field.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF && n == 0 {
				if field.Len() == 0 {
					return "", io.EOF
				}
				return "", fmt.Errorf("truncated")
			}
			return "", err
		}
	}
}

// publishArtifacts пишет бинари и manifest.json во временный каталог и
// атомарно заменяет dir целиком: rename старого в сторону, rename нового,
// удаление старого. Существующий dir не обязан существовать.
func publishArtifacts(dir string, payloads map[string][]byte) ([]byte, error) {
	staging := dir + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("staging directory is unavailable")
	}
	if err := os.MkdirAll(staging, 0755); err != nil {
		return nil, fmt.Errorf("staging directory is unavailable")
	}
	entries := []map[string]any{}
	for _, name := range artifactCommandNames {
		data := payloads[name]
		if err := os.WriteFile(filepath.Join(staging, name), data, 0755); err != nil {
			return nil, fmt.Errorf("staging directory is unavailable")
		}
		sum := sha256.Sum256(data)
		entries = append(entries, map[string]any{"name": name, "size": len(data), "sha256": hex.EncodeToString(sum[:])})
	}
	manifest, err := json.Marshal(map[string]any{"artifacts": entries})
	if err != nil {
		return nil, fmt.Errorf("manifest encoding failed")
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), manifest, 0644); err != nil {
		return nil, fmt.Errorf("staging directory is unavailable")
	}
	if err := os.Rename(dir, dir+".old-"+strconv.Itoa(os.Getpid())); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("publish is unavailable")
	}
	if err := os.Rename(staging, dir); err != nil {
		os.RemoveAll(dir + ".old-" + strconv.Itoa(os.Getpid()))
		return nil, fmt.Errorf("publish is unavailable")
	}
	os.RemoveAll(dir + ".old-" + strconv.Itoa(os.Getpid()))
	return manifest, nil
}
