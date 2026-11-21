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
	catalog "brood.dev/go/brood/command"
	"brood.dev/go/brood/mcp"
	"brood.dev/go/brood/swarmclient"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	return runWithIO(context.Background(), args, os.Stdin, os.Stdout, os.Stderr)
}

// errNotLoggedIn — нет сохранённого сервера или сессии: статус выхода 2.
var errNotLoggedIn = errors.New("not logged in; run: bro login SERVER")

type command struct {
	in              io.Reader
	out, diagnostic io.Writer
	scope           scope
	// session — оператор этого запуска; заводится первым обращением.
	session *operator
}

func runWithIO(ctx context.Context, args []string, in io.Reader, out, diagnostic io.Writer) int {
	options, err := parseExecution(args, os.Getenv("BROOD_EXECUTION_MODE"), os.Getenv("BROOD_SWARM"))
	if err != nil {
		fmt.Fprintf(diagnostic, "bro: %v\n", err)
		return 1
	}
	c := &command{in: in, out: out, diagnostic: diagnostic, scope: options.scope}
	if len(options.args) == 0 {
		overview(diagnostic)
		return 1
	}
	if options.guest {
		err = c.guestCommand(ctx, options.args)
	} else {
		err = c.operatorCommand(ctx, options.args)
	}
	if c.session != nil {
		if saveErr := c.session.save(); err == nil {
			err = saveErr
		}
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, errSilent) {
		return 1
	}
	fmt.Fprintf(diagnostic, "bro: %s\n", describe(err))
	if errors.Is(err, errNotLoggedIn) || errors.Is(err, clientapi.ErrInvalidCredentials) || errors.Is(err, clientapi.ErrSessionExpired) {
		return 2
	}
	return 1
}

// errSilent — отказ, который команда уже объяснила своим выводом.
var errSilent = errors.New("failed")

// describe — отказ одной строкой: причина сервера как есть, без пути запроса.
func describe(err error) string {
	var refusal *clientapi.CommandError
	switch {
	case errors.Is(err, clientapi.ErrSessionExpired):
		return "session expired; run: bro login SERVER"
	case errors.As(err, &refusal) && refusal.Unconfirmed:
		return "the outcome is not confirmed; check before retrying"
	case errors.As(err, &refusal) && refusal.Reason != "":
		return refusal.Reason
	case errors.As(err, &refusal):
		// Без публичной причины сервер называет только род отказа.
		kind := map[int]string{400: "not accepted by the server; its version may differ from bro", 403: "not allowed", 404: "not found"}[refusal.Status]
		if kind == "" {
			kind = "refused by the server"
		}
		return fmt.Sprintf("%s (status %d)", kind, refusal.Status)
	}
	return err.Error()
}

// guestCommand исполняет команду агента: каталог через брокер гостя, без
// сессии оператора и без выбора роя.
func (c *command) guestCommand(ctx context.Context, args []string) error {
	switch args[0] {
	case "build":
		return c.build(ctx, args[1:], true)
	case "help", "-h", "--help":
		catalogUsage(c.out, mcp.GuestSpecs())
		return nil
	}
	if args[0] != "mcp" && !catalogPath(args) {
		return fmt.Errorf("%s is unavailable in guest mode", args[0])
	}
	return c.catalogCommand(ctx, args, swarmclient.Client{}, mcp.GuestSpecs())
}

// operatorCommand исполняет команду человека.
func (c *command) operatorCommand(ctx context.Context, args []string) error {
	name, rest := args[0], args[1:]
	if name == "help" || name == "-h" || name == "--help" {
		return c.help(rest)
	}
	if wantsHelp(name, rest) {
		return c.help(args)
	}
	switch name {
	case "login", "logout":
		return c.auth(ctx, name, rest)
	case "status":
		return c.status(ctx, rest)
	case "projects":
		return c.projects(ctx, rest)
	case "swarms":
		return c.swarms(ctx, rest)
	case "join":
		return c.join(ctx, rest)
	case "swarm":
		return c.swarm(ctx, rest)
	case "agents":
		return c.agents(ctx, rest)
	case "add":
		return c.add(ctx, rest)
	case "rm", "reset":
		return c.destroy(ctx, name, rest)
	case "start", "stop", "restart":
		return c.lifecycle(ctx, name, rest)
	case "send":
		return c.send(ctx, rest)
	case "tail":
		return c.tail(ctx, rest)
	case "attach":
		return c.attach(ctx, rest)
	case "pools":
		return c.pools(ctx, rest)
	case "pool":
		return c.pool(ctx, rest)
	case "build":
		return c.build(ctx, rest, false)
	case "image":
		return c.image(ctx, rest)
	case "secret":
		return c.secret(ctx, rest)
	case "completion":
		return c.completion(rest)
	case "__complete":
		c.complete(ctx, rest)
		return nil
	case "mcp":
		if len(rest) != 0 {
			return errors.New("mcp accepts no arguments")
		}
		return mcp.Serve(ctx, c.in, c.out, operatorExecutor{c}, catalog.Specs())
	}
	if !catalogPath(args) {
		return fmt.Errorf("unknown command %s; see: bro help", name)
	}
	return c.plain(ctx, args)
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
			return nil, nil, errNotLoggedIn
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
