// Build commands use the Go builder directly, without installation/cluster tools.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"brood.dev/go/brood/build"
)

func buildUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro build [target] [options]

Read ./Broodfile and current working files. Synchronize the project, then build locally.
A normal build requires bro login. Registry publication remains separate.

options:
  --project NAME   destination project (or BUILD_PROJECT; default: Broodfile name)
  --plan           print the plan without Docker
  --export PATH    export a new portable build context without Docker

Artifacts are saved under .brood/build/<target>, including image.oci.tar.
Use bro push [target] to upload a completed build to the logged-in server.
--plan and --export are offline, mutually exclusive, and do not synchronize.
Normal builds synchronize the Broodfile project declaration idempotently.
No actors are created. Existing actor checkouts are not changed.
`)
}

func (c command) build(ctx context.Context, args []string, guest bool) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", os.Getenv("BUILD_PROJECT"), "destination project")
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
		if errors.Is(err, flag.ErrHelp) {
			buildUsage(c.out)
			return nil
		}
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
	target := ""
	if len(targets) == 1 {
		target = targets[0]
	}
	selected, err := build.LoadWorkingProject(".", target, *project)
	if err != nil {
		return err
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
	if guest {
		return errors.New("project synchronization requires operator mode; use --plan or --export in guest mode")
	}
	if !selected.RepositoryResolved {
		return errors.New("project repository is unresolved; configure Git origin or set project.repository in Broodfile (an explicit empty string disables cloning)")
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	if client.Stored().Session == "" {
		return errors.New("not logged in; run: bro login <server>")
	}
	declaration, _ := selected.Plan["projectConfig"].(map[string]any)
	synchronized, err := client.ProjectSync(ctx, declaration)
	if err != nil {
		return fmt.Errorf("project synchronization: %w", sessionError(err))
	}
	if synchronized.Repository != "" && synchronized.OriginMode == "" {
		fmt.Fprintln(c.diagnostic, "project synchronized; no Git access mode is configured for its repository")
	}
	if err := sessions.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "bro-build-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	result, err := build.BackendBuildLocal(selected, build.BackendOptions{
		ArchivePath: filepath.Join(temporary, "image.oci.tar"),
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
	output, err := build.PublishLocal(selected.Root, resolvedTarget, artifacts, result.Archive)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.out, result.Image)
	fmt.Fprintln(c.out, output)
	return nil
}
