// bro — пользовательский CLI Brood: вход, проекты и сборка.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"brood.dev/go/brood/clientapi"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 1
	}
	ctx := context.Background()
	switch args[0] {
	case "login":
		return cmdLogin(ctx, args[1:])
	case "logout":
		return cmdLogout(ctx, args[1:])
	case "whoami":
		return cmdWhoami(ctx, args[1:])
	case "projects":
		return cmdProjects(ctx, args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "bro: unknown command %q\n", args[0])
		usage(os.Stderr)
		return 1
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: bro <command> [server] [options]

commands:
  login [server] [--ca FILE]    authenticate against the web API and store the session
  logout [server]               revoke the server session and clear local storage
  whoami [server]               print the current user
  projects list [server]        list projects available to the current user

The server is an origin like https://brood.example; without it the last
logged-in server is used. Sessions are stored per server origin.
`)
}

// parseServer выделяет аргумент server и известные флаги; неизвестный флаг — отказ.
func parseServer(args []string) (server string, caPath string, rest []string, err error) {
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--ca":
			if len(args) < 2 {
				return "", "", nil, fmt.Errorf("--ca requires a file path")
			}
			caPath, args = args[1], args[2:]
		case strings.HasPrefix(arg, "--ca="):
			caPath, args = strings.TrimPrefix(arg, "--ca="), args[1:]
		case strings.HasPrefix(arg, "-"):
			return "", "", nil, fmt.Errorf("unknown option %s", arg)
		default:
			server, args = arg, args[1:]
		}
	}
	return server, caPath, rest, nil
}

// stateDir возвращает каталог сессий: XDG_STATE_HOME, затем
// ~/.local/state, затем пользовательский конфигурационный каталог.
func stateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir + "/brood/sessions"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home + "/.local/state/brood/sessions"
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return dir + "/brood/sessions"
	}
	return ".brood/sessions"
}

func newClient(server string, caPath string) (*clientapi.Client, *clientapi.SessionStore, error) {
	store, err := clientapi.NewSessionStore(stateDir())
	if err != nil {
		return nil, nil, fmt.Errorf("session store: %w", err)
	}
	if server == "" {
		current, err := store.Current()
		if err != nil {
			return nil, nil, fmt.Errorf("no server given and no saved server; run: bro login <server>")
		}
		server = current
	}
	origin, err := clientapi.NormalizeOrigin(server)
	if err != nil {
		return nil, nil, err
	}
	var opts []clientapi.Option
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read CA file: %w", err)
		}
		opts = append(opts, clientapi.WithCACert(pem))
	}
	client, err := clientapi.New(origin, opts...)
	if err != nil {
		return nil, nil, err
	}
	if stored, ok, _ := store.Load(origin); ok {
		client.Restore(stored)
	}
	return client, store, nil
}
