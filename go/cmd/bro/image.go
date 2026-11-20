// bro image lists and deletes worker images built by the server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/clientapi"
)

func imageUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro image list [--project NAME]
       bro image delete IMAGE [--project NAME]

List or delete the worker images the server built for a project. The project
defaults to brood.project of ./brood.yaml. Images are built with bro build.
list prints ID, STATE, REF, POOL, PLAYBOOK and CREATED, newest first.
`)
}

func (c command) image(ctx context.Context, args []string) error {
	if len(args) == 0 {
		imageUsage(c.diagnostic)
		return errors.New("expected list or delete")
	}
	action := args[0]
	flags := flag.NewFlagSet("image", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("project", "", "project")
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
	case action == "delete" && len(positional) == 1 && positional[0] != "":
	case action == "list" || action == "delete":
		return fmt.Errorf("unexpected arguments; see bro image --help")
	default:
		return fmt.Errorf("unknown image action %s; expected list or delete", action)
	}
	if *name == "" {
		declared, err := build.Load(".")
		if err != nil {
			return fmt.Errorf("%w; pass --project NAME", err)
		}
		*name = declared.Name
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	if client.Stored().Session == "" {
		return errors.New("not logged in; run: bro login <server>")
	}
	project, err := findProject(ctx, client, *name)
	if err != nil {
		return sessionError(err)
	}
	if action == "delete" {
		if err := client.ImageDelete(ctx, project, positional[0]); err != nil {
			return sessionError(err)
		}
		fmt.Fprintf(c.out, "image %s deleted\n", positional[0])
		return sessions.Save(client.Origin(), client.Stored())
	}
	images, err := client.Images(ctx, project)
	if err != nil {
		return sessionError(err)
	}
	if len(images) == 0 {
		fmt.Fprintln(c.out, "no images")
	} else {
		table := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "ID\tSTATE\tREF\tPOOL\tPLAYBOOK\tCREATED")
		for _, image := range images {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", image.ID, image.State, image.Ref, image.Pool, image.Playbook, image.CreatedAt)
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	return sessions.Save(client.Origin(), client.Stored())
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
