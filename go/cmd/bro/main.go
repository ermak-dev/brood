// bro is the standalone user CLI for Brood.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"brood.dev/go/brood/clientapi"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	return runWithIO(context.Background(), args, os.Stdin, os.Stdout, os.Stderr)
}

type command struct {
	in              io.Reader
	out, diagnostic io.Writer
}

func runWithIO(ctx context.Context, args []string, in io.Reader, out, diagnostic io.Writer) int {
	c := command{in: in, out: out, diagnostic: diagnostic}
	options, parseErr := parseExecution(args, os.Getenv("BROOD_EXECUTION_MODE"), os.Getenv("BROOD_SWARM"))
	if parseErr != nil {
		fmt.Fprintf(diagnostic, "bro: %v\n", parseErr)
		return 1
	}
	args = options.args
	if len(args) == 0 {
		usage(diagnostic)
		return 1
	}
	var err error
	if options.guest {
		switch args[0] {
		case "login", "logout", "whoami", "projects", "push", "secret", "swarm":
			fmt.Fprintln(diagnostic, "operator command is unavailable in guest mode")
			return 1
		}
	}
	switch args[0] {
	case "login", "logout", "whoami":
		err = c.auth(ctx, args[0], args[1:])
	case "projects":
		rest := args[1:]
		if len(rest) > 0 && rest[0] == "list" {
			rest = rest[1:]
		}
		err = c.auth(ctx, "projects", rest)
	case "build":
		err = c.build(ctx, args[1:], options.guest)
	case "push":
		err = c.push(ctx, args[1:])
	case "secret":
		err = c.secret(ctx, args[1:])
	case "swarm":
		err = c.swarm(ctx, args[1:])
	case "pipeline":
		if options.guest {
			err = c.catalogCommand(ctx, options)
		} else {
			err = c.pipeline(args[1:])
		}
	case "help", "-h", "--help":
		if len(args) != 1 {
			err = errors.New("help accepts no arguments")
		} else {
			usage(out)
		}
	default:
		err = c.catalogCommand(ctx, options)
	}
	if err == nil {
		return 0
	}
	fmt.Fprintf(diagnostic, "bro %s: %v\n", args[0], err)
	if errors.Is(err, clientapi.ErrInvalidCredentials) || errors.Is(err, clientapi.ErrSessionExpired) {
		return 2
	}
	return 1
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: bro <command> [options]

commands:
  login [server] [--ca FILE]      authenticate and store the session
  logout [server] [--ca FILE]     revoke the server session, then clear local storage
  whoami [server] [--ca FILE]     print the current user
  projects list [server] [--ca FILE]  list accessible projects
  build [target] [options]       build current files into local OCI artifacts
  push [target] [options]        upload built images and template to the current server
  secret [options]                manage project secrets on the current server
  swarm roles|bind|unbind SWARM   list and change swarm role bindings to actors
  pipeline <sub> FILE             validate, plan or render a pipeline document
  mcp                           serve catalog tools over stdio
  <catalog command>             execute a scoped orchestration command

Orchestration scope: --swarm ID or BROOD_SWARM. Guest execution must be explicit:
--guest or BROOD_EXECUTION_MODE=guest; broker failure never falls back to operator.

Run bro <command> --help for command-specific options.

Servers use HTTPS (HTTP is allowed only for localhost/loopback development).
Without a server, use the last logged-in origin. Sessions and additional CA trust
are stored per origin under $XDG_STATE_HOME/brood/sessions or ~/.local/state/brood/sessions.
--ca adds a trusted PEM CA and persists it on successful login; TLS verification
cannot be disabled. Passwords are read without echo from a terminal, or as the
second input line when stdin is a pipe. Passwords are never saved.
`)
	catalogUsage(w)
}

func parseServer(args []string) (server, caPath string, rest []string, err error) {
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--ca":
			if len(args) < 2 || args[1] == "" || strings.HasPrefix(args[1], "-") {
				return "", "", nil, errors.New("--ca requires a file path")
			}
			caPath, args = args[1], args[2:]
		case strings.HasPrefix(arg, "--ca="):
			caPath, args = strings.TrimPrefix(arg, "--ca="), args[1:]
			if caPath == "" {
				return "", "", nil, errors.New("--ca requires a file path")
			}
		case strings.HasPrefix(arg, "-"):
			return "", "", nil, fmt.Errorf("unknown option %s", arg)
		default:
			if server != "" {
				return "", "", nil, errors.New("expected at most one server origin")
			}
			server, args = arg, args[1:]
		}
	}
	return server, caPath, nil, nil
}

func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", errors.New("XDG_STATE_HOME must be an absolute path")
		}
		return filepath.Join(dir, "brood", "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("set an absolute XDG_STATE_HOME or HOME for session storage")
	}
	return filepath.Join(home, ".local", "state", "brood", "sessions"), nil
}

func newClient(server, caPath string) (*clientapi.Client, *clientapi.SessionStore, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, nil, err
	}
	store, err := clientapi.NewSessionStore(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("session store: %w", err)
	}
	if server == "" {
		server, err = store.Current()
		if err != nil {
			return nil, nil, err
		}
		if server == "" {
			return nil, nil, errors.New("no saved server; run: bro login <server>")
		}
	}
	origin, err := clientapi.NormalizeOrigin(server)
	if err != nil {
		return nil, nil, err
	}
	stored, _, err := store.Load(origin)
	if err != nil {
		return nil, nil, err
	}
	caPEM := stored.CAPEM
	if caPath != "" {
		data, err := os.ReadFile(caPath)
		if err != nil {
			return nil, nil, errors.New("could not read CA file")
		}
		caPEM = string(data)
	}
	var opts []clientapi.Option
	if caPEM != "" {
		opts = append(opts, clientapi.WithCACert([]byte(caPEM)))
	}
	client, err := clientapi.New(origin, opts...)
	if err != nil {
		return nil, nil, err
	}
	stored.CAPEM = caPEM
	client.Restore(stored)
	return client, store, nil
}
