// Каталог команд — механизм bro: команды человека, MCP и режим гостя
// исполняют одни и те же инструменты сервера.
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
)

func catalogUsage(w io.Writer, specs []catalog.Spec) {
	fmt.Fprintln(w, "usage: bro <command> [arguments]")
	for _, spec := range specs {
		fmt.Fprintf(w, "  %-16s %s\n", strings.Join(spec.Path, " "), spec.Description)
	}
	fmt.Fprintln(w, "\nRun bro <command> --help for its arguments.")
}

// catalogPath сообщает, что аргументы начинаются с пути команды каталога.
func catalogPath(args []string) bool {
	for _, spec := range catalog.Specs() {
		if len(args) >= len(spec.Path) && strings.Join(args[:len(spec.Path)], " ") == strings.Join(spec.Path, " ") {
			return true
		}
	}
	return false
}

// operatorExecutor исполняет инструменты каталога от имени оператора в его
// текущем рое. Рой выбирается на каждый вызов: MCP живёт дольше одного bro join.
type operatorExecutor struct{ c *command }

func (e operatorExecutor) Execute(ctx context.Context, invocation catalog.Invocation) (catalog.Result, error) {
	if invocation.Name == "diagnose" {
		return localDiagnose()
	}
	op, err := e.c.operator()
	if err != nil {
		return catalog.Result{}, err
	}
	executor := clientapi.CommandExecutor{Client: op.client}
	if catalog.NeedsSwarm(invocation.Name) {
		current, err := op.current(ctx)
		if err != nil {
			return catalog.Result{}, err
		}
		executor.SwarmID = current.swarm.ID
	}
	return executor.Execute(ctx, invocation)
}

func localDiagnose() (catalog.Result, error) {
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

// invoke исполняет инструмент каталога с аргументами команды человека.
func (c *command) invoke(ctx context.Context, tool string, arguments map[string]any) (catalog.Result, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return catalog.Result{}, err
	}
	invocation, err := catalog.ParseTool(tool, raw)
	if err != nil {
		return catalog.Result{}, err
	}
	return operatorExecutor{c}.Execute(ctx, invocation)
}

// plain исполняет команду каталога, у которой нет своего вывода для
// человека: текст результата и его данные.
func (c *command) plain(ctx context.Context, args []string) error {
	invocation, err := parseCatalogCLI(args)
	if err != nil {
		return fmt.Errorf("%w; see: bro %s --help", err, args[0])
	}
	result, err := operatorExecutor{c}.Execute(ctx, invocation)
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
		if err := writeJSON(c.out, value); err != nil {
			return err
		}
	}
	if result.Outcome == "unconfirmed" {
		return errors.New("the outcome is not confirmed; check before retrying")
	}
	return nil
}

func catalogCommandHelp(w io.Writer, args []string, specs []catalog.Spec) error {
	path := strings.Join(args, " ")
	for _, spec := range specs {
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

// catalogCommand — каталог как он есть: путь гостя и его MCP. Вывод читает
// модель, поэтому он не меняется вместе с командами человека.
func (c *command) catalogCommand(ctx context.Context, args []string, executor catalog.Executor, specs []catalog.Spec) error {
	if len(args) > 1 && args[len(args)-1] == "--help" {
		return catalogCommandHelp(c.out, args[:len(args)-1], specs)
	}
	if args[0] == "mcp" {
		if len(args) != 1 {
			return errors.New("mcp accepts no command arguments")
		}
		return mcp.Serve(ctx, c.in, c.out, executor, specs)
	}
	invocation, err := parseCatalogCLI(args)
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
