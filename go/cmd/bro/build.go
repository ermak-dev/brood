// Build commands use the Go builder directly, without installation/cluster tools.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"brood.dev/go/brood/build"
)

func (c command) build(args []string) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", ".", "Git project directory")
	ref := flags.String("ref", "HEAD", "committed revision")
	registry := flags.String("registry", os.Getenv("BUILD_REGISTRY"), "publication destination")
	atespace := flags.String("atespace", os.Getenv("BUILD_ATESPACE"), "artifact namespace")
	planOnly := flags.Bool("plan", false, "print plan")
	export := flags.String("export", "", "export context")
	// flag останавливается на первом позиционном аргументе: выделяем target,
	// сохраняя пары флаг-значение, в том числе --option=value.
	var options, targets []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			targets = append(targets, arg)
			continue
		}
		options = append(options, arg)
		name, _, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if name != "plan" && !hasValue && flags.Lookup(name) != nil {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return fmt.Errorf("--%s requires a value", name)
			}
			i++
			options = append(options, args[i])
		}
	}
	if err := flags.Parse(options); err != nil {
		return err
	}
	if len(targets) > 1 {
		return errors.New("build accepts at most one target")
	}
	if len(flags.Args()) > 0 {
		return errors.New("unexpected build arguments")
	}
	if *planOnly && *export != "" {
		return errors.New("--plan and --export are mutually exclusive")
	}
	exportSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "export" {
			exportSet = true
		}
	})
	if exportSet && *export == "" {
		return errors.New("--export requires a non-empty path")
	}
	if *registry == "" {
		return errors.New("set --registry or BUILD_REGISTRY explicitly")
	}
	if *atespace == "" {
		return errors.New("set --atespace or BUILD_ATESPACE explicitly")
	}
	target := ""
	if len(targets) == 1 {
		target = targets[0]
	}
	selected, err := build.LoadProject(*project, *ref, target, *registry, *atespace)
	if err != nil {
		return err
	}
	if selected.Dirty {
		fmt.Fprintln(c.diagnostic, "working-tree changes excluded; using committed sources")
	}
	if *planOnly {
		encoder := json.NewEncoder(c.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(selected.Plan)
	}
	if *export != "" {
		if _, err := build.Export(selected, *export, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, *export)
		return nil
	}
	// Явные CLI/env-настройки разрешают только выбранное назначение публикации.
	// Проверка кластера здесь не нужна: клиент не меняет ни кластер, ни локальные
	// настройки Docker. BackendBuild выполняет только build и push.
	authorizedRegistry := *registry
	result, err := build.BackendBuild(selected, build.BackendOptions{
		RegistryAllowlist: []string{authorizedRegistry},
		OwnershipCheck: func() error {
			if selected.Plan["registry"] != authorizedRegistry {
				return errors.New("build registry differs from the authorized publication destination")
			}
			return nil
		},
	})
	if err != nil {
		var buildErr *build.BuildError
		if errors.As(err, &buildErr) {
			fmt.Fprint(c.diagnostic, buildErr.Stdout, buildErr.Stderr)
		}
		return err
	}
	fmt.Fprint(c.diagnostic, result.Stdout, result.Stderr)
	artifacts, err := build.Render(selected.Plan, result.Image, result.Sources)
	if err != nil {
		return err
	}
	resolvedTarget, _ := selected.Plan["target"].(string)
	output, err := build.Publish(selected.Root, resolvedTarget, artifacts)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.out, result.Image)
	fmt.Fprintln(c.out, output)
	return nil
}
