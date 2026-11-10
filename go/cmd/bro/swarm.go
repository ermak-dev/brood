package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"brood.dev/go/brood/clientapi"
)

func swarmUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro swarm roles SWARM [--definition NAME]
       bro swarm bind SWARM ROLE ACTOR
       bro swarm unbind SWARM ROLE

Operator-only command. SWARM is a swarm ID or a name unique among accessible
swarms; ACTOR is a managed actor name or UID of that swarm.

roles prints the swarm execution, its graph and one row per role of the graph
or of a binding: ROLE, GRAPH (yes, no, or - without a graph), STATE, ACTOR,
UID and INCARNATION. The graph is the pinned snapshot of the swarm; before the
first start it is the catalog workflow named by --definition. STATE is ready
when the bound actor would take the role at a swarm start; otherwise unbound,
stale (the actor was recreated; bind it again), inactive (not running or not
meant to run), foreign (the actor left the swarm) or missing (the actor is
gone). Pools are not checked here.

bind binds the role to the actor's current incarnation; on an existing
binding it rebinds. unbind removes the binding. Bindings change only while
the swarm execution is neither running nor waiting: stop the swarm first.
Viewers may list roles; owners, editors and administrators change them.
`)
}

type swarmOptions struct {
	action, swarm, role, actor, definition string
}

func parseSwarm(args []string) (swarmOptions, error) {
	var o swarmOptions
	var positional []string
	definitionSet := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--definition" || strings.HasPrefix(arg, "--definition="):
			if definitionSet {
				return o, errors.New("definition must be specified once")
			}
			definitionSet = true
			if value, ok := strings.CutPrefix(arg, "--definition="); ok {
				o.definition = value
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				o.definition = args[i]
			}
			if o.definition == "" {
				return o, errors.New("--definition requires a workflow name")
			}
		case strings.HasPrefix(arg, "-"):
			return o, errors.New("unknown swarm option; use bro swarm --help")
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) == 0 {
		return o, errors.New("swarm requires roles, bind or unbind")
	}
	o.action = positional[0]
	want := map[string]int{"roles": 2, "bind": 4, "unbind": 3}[o.action]
	if want == 0 {
		return o, errors.New("swarm action must be roles, bind or unbind")
	}
	if len(positional) != want {
		return o, errors.New("usage: bro swarm roles SWARM | bind SWARM ROLE ACTOR | unbind SWARM ROLE")
	}
	if definitionSet && o.action != "roles" {
		return o, errors.New("--definition is only valid for swarm roles")
	}
	o.swarm = positional[1]
	if want > 2 {
		o.role = positional[2]
	}
	if want > 3 {
		o.actor = positional[3]
	}
	for _, value := range positional[1:] {
		if value == "" {
			return o, errors.New("swarm, role and actor must not be empty")
		}
	}
	return o, nil
}

// swarm — bro swarm: привязки ролей роя к managed-акторам (#235).
func (c command) swarm(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		swarmUsage(c.out)
		return nil
	}
	o, err := parseSwarm(args)
	if err != nil {
		return err
	}
	client, store, err := newClient("", "")
	if err != nil {
		return err
	}
	swarm, err := resolveSwarm(ctx, client, o.swarm)
	if err != nil {
		return err
	}
	var view clientapi.SwarmRoles
	switch o.action {
	case "roles":
		view, err = client.SwarmRoles(ctx, swarm.ID, o.definition)
	case "bind":
		var actor clientapi.SwarmActor
		if actor, err = resolveSwarmActor(ctx, client, swarm.ID, o.actor); err == nil {
			view, err = client.BindSwarmRole(ctx, swarm.ID, o.role, actor.UID, actor.Incarnation)
		}
	default:
		view, err = client.UnbindSwarmRole(ctx, swarm.ID, o.role)
	}
	if err != nil {
		return sessionError(err)
	}
	if err := store.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	switch o.action {
	case "roles":
		printSwarmRoles(c.out, swarm, view)
	case "bind":
		for _, role := range view.Roles {
			if role.Role == o.role {
				fmt.Fprintf(c.out, "bound %s to %s (uid %s, incarnation %d): %s\n", role.Role, dash(role.ActorName), role.ActorUID, role.ActorIncarnation, role.State)
				return nil
			}
		}
		fmt.Fprintf(c.out, "bound %s\n", o.role)
	default:
		fmt.Fprintf(c.out, "unbound %s\n", o.role)
	}
	return nil
}

func printSwarmRoles(w io.Writer, swarm clientapi.Swarm, view clientapi.SwarmRoles) {
	fmt.Fprintf(w, "swarm %s (%s): execution %s\n", swarm.ID, swarm.Name, view.Execution)
	if g := view.Graph; g == nil {
		fmt.Fprintln(w, "graph: none; name a catalog workflow with --definition")
	} else {
		source := "catalog"
		if g.Pinned {
			source = "pinned"
		}
		fmt.Fprintf(w, "graph: %s version %d revision %s (%s)\n", g.Name, g.Version, g.Revision, source)
	}
	if len(view.Roles) == 0 {
		fmt.Fprintln(w, "no roles")
		return
	}
	fmt.Fprintln(w, "ROLE\tGRAPH\tSTATE\tACTOR\tUID\tINCARNATION")
	for _, role := range view.Roles {
		inGraph := "-"
		if view.Graph != nil {
			inGraph = map[bool]string{true: "yes", false: "no"}[role.InGraph]
		}
		incarnation := "-"
		if role.ActorIncarnation > 0 {
			incarnation = strconv.FormatInt(role.ActorIncarnation, 10)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", role.Role, inGraph, role.State, dash(role.ActorName), dash(role.ActorUID), incarnation)
	}
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// resolveSwarm находит рой по ID или по имени среди доступных роёв всех
// проектов; удалённые рои по имени не находятся, неоднозначное имя — отказ.
func resolveSwarm(ctx context.Context, client *clientapi.Client, selected string) (clientapi.Swarm, error) {
	projects, err := client.ProjectsList(ctx)
	if err != nil {
		return clientapi.Swarm{}, sessionError(err)
	}
	var named []clientapi.Swarm
	for _, project := range projects {
		swarms, err := client.SwarmsList(ctx, project.ID)
		if err != nil {
			return clientapi.Swarm{}, sessionError(err)
		}
		for _, swarm := range swarms {
			if swarm.ID == selected {
				return swarm, nil
			}
			if swarm.Name == selected && swarm.State != "deleted" {
				named = append(named, swarm)
			}
		}
	}
	switch len(named) {
	case 0:
		return clientapi.Swarm{}, errors.New("swarm not found or not accessible")
	case 1:
		return named[0], nil
	default:
		return clientapi.Swarm{}, errors.New("swarm name is ambiguous; use its ID")
	}
}

// resolveSwarmActor находит managed-актора роя по UID или по имени.
func resolveSwarmActor(ctx context.Context, client *clientapi.Client, swarm, selected string) (clientapi.SwarmActor, error) {
	actors, err := client.SwarmActors(ctx, swarm)
	if err != nil {
		return clientapi.SwarmActor{}, err
	}
	for _, actor := range actors {
		if actor.UID == selected {
			return actor, nil
		}
	}
	for _, actor := range actors {
		if actor.Name == selected {
			return actor, nil
		}
	}
	return clientapi.SwarmActor{}, errors.New("actor not found in the swarm; only managed actors hold roles: list them with bro --swarm SWARM agents")
}
