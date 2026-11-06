// Pipeline commands work on local pipeline documents without a server.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/piprender"
)

func pipelineUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro pipeline <subcommand> FILE [options]

Validate, plan or render a declarative pipeline document (brood.dev/v1alpha1).
Use --workflow NAME [Broodfile] to select a workflow (default: ./Broodfile).

subcommands:
  validate FILE      print diagnostics (ERROR/WARNING/INFO); ERROR lines fail
  plan FILE          simulate the walk: every node succeeds, human gates pick
                     the first option; prints the node sequence and chosen edges
  render FILE --dot|--svg  print a Graphviz DOT or standalone SVG graph
`)
}

// loadPipeline читает и валидирует документ конвейера.
func parsePipeline(path, workflow string) (*pipeline.Document, error) {
	if workflow != "" {
		if path == "" {
			path = "Broodfile"
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		document, err := build.ParseDocument(string(data), path)
		if err != nil {
			return nil, err
		}
		return build.Workflow(document, workflow, path)
	}
	return pipeline.ParseFile(path)
}

func loadPipeline(path, workflow string) (*pipeline.Document, error) {
	document, err := parsePipeline(path, workflow)
	if err != nil {
		return nil, err
	}
	if err := document.Validate(); err != nil {
		return nil, err
	}
	return document, nil
}

func (c command) pipeline(args []string) error {
	if len(args) == 0 {
		pipelineUsage(c.out)
		return errors.New("pipeline requires a subcommand")
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "help", "--help", "-h":
		pipelineUsage(c.out)
		return nil
	case "validate":
		return c.pipelineValidate(rest)
	case "plan":
		return c.pipelinePlan(rest)
	case "render":
		return c.pipelineRender(rest)
	default:
		pipelineUsage(c.diagnostic)
		return fmt.Errorf("unknown pipeline subcommand %q", subcommand)
	}
}

// pipelineOptions извлекает имя workflow и оставляет прочие аргументы без изменений.
func pipelineOptions(args []string) (string, []string, error) {
	var workflow string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--workflow" {
			if workflow != "" || i+1 == len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return "", nil, errors.New("--workflow requires one name")
			}
			i++
			workflow = args[i]
		} else {
			rest = append(rest, args[i])
		}
	}
	return workflow, rest, nil
}

// pipelineFile достаёт единственный позиционный аргумент FILE.
func pipelineFile(subcommand string, args []string, workflow string) (string, error) {
	file := ""
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			if file != "" {
				return "", fmt.Errorf("pipeline %s accepts exactly one file", subcommand)
			}
			file = arg
		}
	}
	if file == "" && workflow == "" {
		return "", fmt.Errorf("pipeline %s requires a document file", subcommand)
	}
	return file, nil
}

func (c command) pipelineValidate(args []string) error {
	workflow, args, err := pipelineOptions(args)
	if err != nil {
		return err
	}
	if err := rejectFlags("validate", args); err != nil {
		return err
	}
	file, err := pipelineFile("validate", args, workflow)
	if err != nil {
		return err
	}
	document, err := parsePipeline(file, workflow)
	if err != nil {
		return err
	}
	diagnostics, err := document.Diagnostics()
	if err != nil {
		return err
	}
	failed := false
	for _, d := range diagnostics {
		fmt.Fprintf(c.out, "%s %s\n", d.Severity, d.Message)
		if d.Severity == pipeline.ERROR {
			failed = true
		}
	}
	if failed {
		return errors.New("pipeline document has errors")
	}
	return nil
}

func (c command) pipelinePlan(args []string) error {
	workflow, args, err := pipelineOptions(args)
	if err != nil {
		return err
	}
	if err := rejectFlags("plan", args); err != nil {
		return err
	}
	file, err := pipelineFile("plan", args, workflow)
	if err != nil {
		return err
	}
	document, err := loadPipeline(file, workflow)
	if err != nil {
		return err
	}
	steps, err := document.Simulate()
	if err != nil {
		return err
	}
	for _, step := range steps {
		fmt.Fprintf(c.out, "%s -> %s\n", step.Node, step.Next)
	}
	if len(steps) > 0 {
		fmt.Fprintf(c.out, "exit at %s\n", steps[len(steps)-1].Next)
	}
	return nil
}

func (c command) pipelineRender(args []string) error {
	workflow, args, err := pipelineOptions(args)
	if err != nil {
		return err
	}
	format := ""
	var rest []string
	for _, arg := range args {
		switch arg {
		case "--dot", "--svg":
			if format != "" {
				return errors.New("pipeline render requires exactly one of --dot or --svg")
			}
			format = arg
		default:
			rest = append(rest, arg)
		}
	}
	if format == "" {
		return errors.New("pipeline render requires --dot or --svg")
	}
	if err := rejectUnknownRenderFlags(args); err != nil {
		return err
	}
	file, err := pipelineFile("render", rest, workflow)
	if err != nil {
		return err
	}
	document, err := loadPipeline(file, workflow)
	if err != nil {
		return err
	}
	if format == "--svg" {
		fmt.Fprint(c.out, piprender.SVG(piprender.RunView{Graph: document.Graph()}))
	} else {
		fmt.Fprint(c.out, piprender.DOT(document.Graph()))
	}
	return nil
}

// rejectFlags запрещает любые флаги для подкоманд без опций.
func rejectFlags(subcommand string, args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("pipeline %s accepts no options: %s", subcommand, arg)
		}
	}
	return nil
}

// rejectUnknownRenderFlags разрешает только --dot.
func rejectUnknownRenderFlags(args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") && arg != "--dot" && arg != "--svg" {
			return fmt.Errorf("unknown pipeline render option %s", arg)
		}
	}
	return nil
}
