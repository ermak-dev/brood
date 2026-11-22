// bro image lists and removes agent images built by the server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/clientapi"
)

func imageUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro image list [--project NAME] [--json]
       bro image rm IMAGE [--project NAME]

List or remove the agent images the server built for a project. New agents
are clones of the newest ready image. Images are built with bro build.

  IMAGE            image ID from bro image list
  --project NAME   project (default: brood.project of ./brood.yaml)
  --json           print the list as JSON

list prints ID, STATE, REF, POOL, PLAYBOOK and AGE, newest first.

example: bro image list
`)
}

func (c *command) image(ctx context.Context, args []string) error {
	if len(args) == 0 {
		imageUsage(c.diagnostic)
		return errors.New("expected: bro image list or bro image rm IMAGE")
	}
	action := args[0]
	flags := flag.NewFlagSet("image", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("project", "", "project")
	asJSON := flags.Bool("json", false, "print JSON")
	// flag останавливается на первом позиционном аргументе: IMAGE может
	// стоять и до, и после --project.
	var positional []string
	rest := args[1:]
	for {
		if err := flags.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				imageUsage(c.out)
				return nil
			}
			return err
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	switch {
	case action == "-h" || action == "--help" || action == "help":
		imageUsage(c.out)
		return nil
	case action == "list" && len(positional) == 0:
	case action == "rm" && len(positional) == 1 && positional[0] != "":
	case action == "list" || action == "rm":
		return fmt.Errorf("unexpected arguments; see: bro image --help")
	default:
		return fmt.Errorf("unknown image action %s; expected list or rm", action)
	}
	if *name == "" {
		declared, err := build.Load(".")
		if err != nil {
			return fmt.Errorf("%w; pass --project NAME", err)
		}
		*name = declared.Name
	}
	op, err := c.operator()
	if err != nil {
		return err
	}
	project, err := findProject(ctx, op.client, *name)
	if err != nil {
		return err
	}
	if action == "rm" {
		if err := op.client.ImageDelete(ctx, project, positional[0]); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "removed image %s\n", positional[0])
		return nil
	}
	images, err := op.client.Images(ctx, project)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(c.out, append([]clientapi.Image{}, images...))
	}
	if len(images) == 0 {
		fmt.Fprintln(c.out, "no images; build one with: bro build")
		return nil
	}
	list := table{header: []string{"ID", "STATE", "SNAPSHOT", "POOL", "PLAYBOOK", "AGE"}}
	now := time.Now()
	for _, image := range images {
		tone := toneNone
		if image.State == clientapi.ImageFailed {
			tone = toneRed
		}
		list.add(tone, image.ID, image.State, image.Snapshot, image.Pool, image.Playbook, age(image.CreatedAt, now))
	}
	return list.write(c.out)
}

// findProject переводит имя или ID проекта в ID среди доступных проектов.
func findProject(ctx context.Context, client *clientapi.Client, name string) (string, error) {
	projects, err := client.ProjectsList(ctx)
	if err != nil {
		return "", err
	}
	for _, project := range projects {
		if project.Name == name || project.ID == name {
			return project.ID, nil
		}
	}
	return "", fmt.Errorf("project %s is not found or not accessible", name)
}
