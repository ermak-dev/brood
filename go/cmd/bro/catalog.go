package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"brood.dev/go/brood/clientapi"
	catalog "brood.dev/go/brood/command"
	"brood.dev/go/brood/mcp"
	"brood.dev/go/brood/swarmclient"
)

func catalogUsage(w io.Writer) {
	fmt.Fprintln(w, "\nOrchestration commands:")
	for _, spec := range catalog.Specs() {
		fmt.Fprintf(w, "  %-24s %s\n", strings.Join(spec.Path, " "), spec.Description)
	}
}

type operatorExecutor struct{ swarm string }

func (e operatorExecutor) Execute(ctx context.Context, invocation catalog.Invocation) (catalog.Result, error) {
	if invocation.Name == "diagnose" {
		dir, err := stateDir()
		if err != nil {
			return catalog.Result{}, err
		}
		info, err := clientapi.DiagnoseSessions(dir)
		if err != nil {
			return catalog.Result{}, err
		}
		data, _ := json.Marshal(info)
		return catalog.Result{Text: "Offline local client diagnostics; no authentication or network probe was performed.", Data: data, Outcome: "completed"}, nil
	}
	if e.swarm == "" && operatorCommandNeedsSwarm(invocation.Name) {
		return catalog.Result{}, errors.New("select a swarm with --swarm or BROOD_SWARM")
	}
	client, store, err := newClient("", "")
	if err != nil {
		return catalog.Result{}, err
	}
	result, err := (clientapi.CommandExecutor{Client: client, SwarmID: e.swarm}).Execute(ctx, invocation)
	if err == nil {
		err = store.Save(client.Origin(), client.Stored())
	}
	return result, err
}

func operatorCommandNeedsSwarm(name string) bool {
	return catalog.NeedsSwarm(name)
}

func catalogCommandHelp(w io.Writer, args []string) error {
	path := strings.Join(args, " ")
	for _, spec := range catalog.Specs() {
		if strings.Join(spec.Path, " ") != path {
			continue
		}
		fmt.Fprintf(w, "bro %s\n%s\n", path, spec.Description)
		for _, arg := range spec.Args {
			name := "--" + strings.ReplaceAll(arg.Name, "_", "-")
			if arg.Positional {
				name = arg.Name
			}
			fmt.Fprintf(w, "  %-20s %s", name, arg.Type)
			if arg.Required {
				fmt.Fprint(w, " (required)")
			}
			if arg.Default != nil {
				fmt.Fprintf(w, " (default: %v)", arg.Default)
			}
			fmt.Fprintln(w)
		}
		return nil
	}
	return errors.New("unknown command path")
}

func (c command) catalogCommand(ctx context.Context, options executionOptions) error {
	if len(options.args) > 1 && options.args[len(options.args)-1] == "--help" {
		return catalogCommandHelp(c.out, options.args[:len(options.args)-1])
	}
	var executor catalog.Executor = operatorExecutor{swarm: options.swarm}
	specs := catalog.Specs()
	if options.guest {
		executor = swarmclient.Client{}
		specs = mcp.GuestSpecs()
	}
	args := options.args
	if args[0] == "mcp" {
		if len(args) != 1 {
			return errors.New("mcp accepts no command arguments")
		}
		return mcp.Serve(ctx, c.in, c.out, executor, specs)
	}
	invocation, err := catalog.ParseCLI(args)
	if err != nil {
		return err
	}
	result, err := executor.Execute(ctx, invocation)
	if err != nil {
		return err
	}
	if result.Text != "" {
		fmt.Fprintln(c.out, result.Text)
	}
	if len(result.Data) > 0 {
		var value any
		if json.Unmarshal(result.Data, &value) != nil {
			return errors.New("invalid command result data")
		}
		if err := json.NewEncoder(c.out).Encode(value); err != nil {
			return err
		}
	}
	if result.Outcome != "" {
		fmt.Fprintf(c.out, "outcome: %s\n", result.Outcome)
	}
	if result.Outcome == "unconfirmed" {
		return errors.New("command outcome is unconfirmed; check before retrying")
	}
	return nil
}
