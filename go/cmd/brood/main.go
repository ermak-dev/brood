// Command brood — серверный бинарник Brood: serve, bootstrap-admin,
// version. Настройки — переменные окружения (brood/server.Config);
// выход: 0 — успех, 1 — ошибка использования/настроек.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/server"
	"brood.dev/go/brood/store"
)

// version — версия бинарника; фиксируется сборкой (-ldflags).
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run разбирает аргументы и исполняет подкоманду; возвращает код выхода.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 1
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(rest, stderr)
	case "bootstrap-admin":
		return cmdBootstrapAdmin(rest, stdin, stdout, stderr)
	case "database":
		return cmdDatabase(rest, stdout, stderr)
	case "fixture":
		return cmdFixture(rest, stdin, stdout, stderr)
	case "artifacts":
		return cmdArtifacts(rest, stdin, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		usage(stderr)
		return 0
	default:
		fmt.Fprintf(stderr, "brood: unknown command %q\n", cmd)
		usage(stderr)
		return 1
	}
}

func usage(stderr io.Writer) {
	fmt.Fprint(stderr, `usage:
  brood serve [--env-file FILE]  run the server
  brood bootstrap-admin     create the first administrator interactively
  brood fixture export [--swarm PROJECT/SWARM]  print the installation fixture as JSON
  brood fixture apply       insert the installation fixture JSON from stdin
  brood version             print the binary version

settings: BROOD_DB_PATH, BROOD_ADDR, BROOD_TLS_CERT, BROOD_TLS_KEY,
  BROOD_PUBLIC_ORIGIN, BROOD_PROXMOX_RELAY_SECRET, BROOD_ARTIFACTS_DIR,
  BROOD_CLIPROXY_CONFIG, CLIPROXY_API_KEY, CLIPROXY_MANAGEMENT_KEY
`)
}

// cmdServe поднимает HTTP-сервер и завершается по SIGTERM/SIGINT:
// слушатель закрывается с ожиданием активных запросов, затем база.
func cmdServe(args []string, stderr io.Writer) int {
	get, err := serveSettings(args)
	if err != nil {
		fmt.Fprintf(stderr, "brood serve: %s\n", err)
		return 1
	}
	cfg, err := server.LoadConfig(get)
	if err != nil {
		fmt.Fprintf(stderr, "brood serve: %s\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// Временный режим выкладки (BROOD_DEPLOY_MODE=fixture): новая база на релиз.
	if generation := get("BROOD_DB_RESET"); generation != "" {
		reset, err := resetDatabase(ctx, cfg.DBPath, generation, get("BROOD_FIXTURE_FILE"))
		if err != nil {
			fmt.Fprintf(stderr, "brood serve: database reset: %s\n", err)
			return 1
		}
		if reset {
			fmt.Fprintf(stderr, "brood serve: database recreated for %s\n", generation)
		}
	}
	if err := server.Serve(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "brood serve: %s\n", err)
		return 1
	}
	return 0
}

func serveSettings(args []string) (func(string) string, error) {
	if len(args) == 0 {
		return os.Getenv, nil
	}
	if len(args) != 2 || args[0] != "--env-file" || !filepath.IsAbs(args[1]) {
		return nil, errors.New("expected --env-file with an absolute path")
	}
	f, err := os.Open(args[1])
	if err != nil {
		return nil, errors.New("could not open --env-file")
	}
	defer f.Close()
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(data) > limit {
		return nil, errors.New("could not read --env-file within its 2 MiB limit")
	}
	settings := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if ok && (strings.HasPrefix(name, "BROOD_") || strings.HasPrefix(name, "CLIPROXY_")) {
			settings[name] = strings.TrimSpace(value)
		}
	}
	return func(name string) string { return settings[name] }, nil
}

// cmdBootstrapAdmin интерактивно заводит первого администратора: пароль
// вводится без эха, не аргументом процесса и не дефолтом.
func cmdBootstrapAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "brood bootstrap-admin: unexpected arguments")
		return 1
	}
	cfg, err := server.LoadConfigEnv()
	if err != nil {
		fmt.Fprintf(stderr, "brood bootstrap-admin: %s\n", err)
		return 1
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		fmt.Fprintf(stderr, "brood bootstrap-admin: %s\n", err)
		return 1
	}
	defer st.Close()

	username, err := readLine(stdin, "username: ", stdout)
	if err != nil {
		fmt.Fprintf(stderr, "brood bootstrap-admin: %s\n", err)
		return 1
	}
	if username == "" {
		fmt.Fprintln(stderr, "brood bootstrap-admin: username must not be empty")
		return 1
	}
	password, err := readPassword(stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "brood bootstrap-admin: %s\n", err)
		return 1
	}
	hash, err := auth.HashPassword(password, auth.DefaultArgon2())
	if err != nil {
		fmt.Fprintf(stderr, "brood bootstrap-admin: %s\n", err)
		return 1
	}
	created, err := st.BootstrapAdmin(ctx, username, hash)
	if err != nil {
		fmt.Fprintf(stderr, "brood bootstrap-admin: %s\n", err)
		return 1
	}
	if created {
		fmt.Fprintf(stdout, "admin %s created\n", username)
	} else {
		fmt.Fprintf(stdout, "user %s already exists; nothing changed\n", username)
	}
	return 0
}

// readPassword читает пароль без эха; терминал — с выключением эха,
// нетерминальный ввод — построчно (приглашение уже напечатано).
func readPassword(stdin io.Reader, stdout io.Writer) (string, error) {
	fmt.Fprint(stdout, "password: ")
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		return readPasswordTerminal(f, os.Stdout)
	}
	return readLine(stdin, "", nil)
}
