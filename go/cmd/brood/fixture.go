package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"brood.dev/go/brood/server"
	"brood.dev/go/brood/store"
)

// cmdFixture: export печатает фикстуру установки из текущей базы, apply
// читает её из stdin и вставляет в базу.
func cmdFixture(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "export" && args[0] != "apply") {
		fmt.Fprintln(stderr, "brood fixture: expected export or apply")
		return 1
	}
	cfg, err := server.LoadConfigEnv()
	if err != nil {
		fmt.Fprintf(stderr, "brood fixture: %s\n", err)
		return 1
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		fmt.Fprintf(stderr, "brood fixture: %s\n", err)
		return 1
	}
	defer st.Close()
	if args[0] == "export" {
		f, err := st.ExportFixture(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "brood fixture: %s\n", err)
			return 1
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(f); err != nil {
			fmt.Fprintf(stderr, "brood fixture: %s\n", err)
			return 1
		}
		return 0
	}
	data, err := readFixture(stdin)
	if err == nil {
		err = applyFixture(ctx, st, data)
	}
	if err != nil {
		fmt.Fprintf(stderr, "brood fixture: %s\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "fixture applied")
	return 0
}

func applyFixture(ctx context.Context, st *store.Store, data []byte) error {
	f, err := store.DecodeFixture(data)
	if err != nil {
		return err
	}
	return st.ApplyFixture(ctx, f)
}

func readFixture(r io.Reader) ([]byte, error) {
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && len(data) > limit {
		return nil, errors.New("fixture exceeds 1 MiB")
	}
	return data, err
}

var resetGeneration = regexp.MustCompile(`^[0-9A-Za-z]{8,64}$`)

// resetDatabase пересоздаёт базу один раз на поколение (sha релиза): прежняя
// база уходит в единственную копию <db>.before-<поколение>, новая получает
// фикстуру, метка <db>.reset запоминает поколение. Перезапуск службы того же
// релиза базу не трогает.
func resetDatabase(ctx context.Context, path, generation, fixturePath string) (bool, error) {
	if !resetGeneration.MatchString(generation) {
		return false, errors.New("BROOD_DB_RESET must be 8-64 ASCII letters or digits")
	}
	marker := path + ".reset"
	done, err := os.ReadFile(marker)
	if err == nil && strings.TrimSpace(string(done)) == generation {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if !filepath.IsAbs(fixturePath) {
		return false, errors.New("BROOD_FIXTURE_FILE must be an absolute path")
	}
	file, err := os.Open(fixturePath)
	if err != nil {
		return false, errors.New("could not open BROOD_FIXTURE_FILE")
	}
	data, err := readFixture(file)
	file.Close()
	if err != nil {
		return false, fmt.Errorf("read BROOD_FIXTURE_FILE: %w", err)
	}
	fixture, err := store.DecodeFixture(data)
	if err != nil {
		return false, err
	}
	backup := path + ".before-" + generation
	_, err = os.Stat(backup)
	switch {
	case os.IsNotExist(err):
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Rename(path+suffix, backup+suffix); err != nil && !os.IsNotExist(err) {
				return false, err
			}
		}
	case err == nil:
		// Прерванный сброс этого поколения: копия уже есть, недостроенная база — нет.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				return false, err
			}
		}
	default:
		return false, err
	}
	older, _ := filepath.Glob(path + ".before-*")
	for _, name := range older {
		if !strings.HasPrefix(name, backup) {
			os.Remove(name)
		}
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return false, err
	}
	err = st.ApplyFixture(ctx, fixture)
	if closeErr := st.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	tmp := marker + ".tmp"
	if err := os.WriteFile(tmp, []byte(generation+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, marker)
}
