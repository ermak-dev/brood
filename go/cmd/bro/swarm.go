package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"brood.dev/go/brood/clientapi"
)

func swarmUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro swarm roles SWARM [--definition NAME]
       bro swarm bind SWARM ROLE ACTOR
       bro swarm unbind SWARM ROLE
       bro swarm start SWARM --definition NAME (--goal TEXT | --goal-file FILE) [--commit SHA]
       bro swarm stop SWARM

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

start begins a new pass of the swarm graph with its goal: the operator's task
that every step instruction of the pass carries, up to 4096 bytes of UTF-8
text without control characters other than newline and tab. --goal-file reads
it from FILE or, with -, from stdin and drops one trailing newline. The goal
is text for the actors, not a permission boundary. The first start pins the
catalog workflow named by --definition and the project repository. Every
start pins the commit of its pass: --commit SHA, or the commit the project
branch points to now. A restart names the same workflow and may run on a new
commit; earlier passes keep theirs. stop freezes the running or waiting pass;
actors, their workspaces and role bindings stay. Both print the execution,
its graph, source and last step. Owners, editors and administrators start and
stop a swarm.
`)
}

type swarmOptions struct {
	action, swarm, role, actor string
	definition, goal, goalFile string
	commit                     string
}

// swarmFlags — флаги bro swarm: что значение называет в отказе и действия,
// которым флаг разрешён.
var swarmFlags = map[string]struct {
	value   string
	actions []string
}{
	"definition": {"a workflow name", []string{"roles", "start"}},
	"goal":       {"the goal text", []string{"start"}},
	"goal-file":  {"a file or -", []string{"start"}},
	"commit":     {"a commit", []string{"start"}},
}

func parseSwarm(args []string) (swarmOptions, error) {
	var o swarmOptions
	var positional []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		flag, known := swarmFlags[name]
		if !known || !strings.HasPrefix(arg, "--") {
			return o, errors.New("unknown swarm option; use bro swarm --help")
		}
		if _, repeated := flags[name]; repeated {
			return o, fmt.Errorf("--%s must be specified once", name)
		}
		// Значение, начинающееся с «-», идёт только как --flag=VALUE: иначе
		// пропущенное значение съело бы следующий флаг.
		if !inline && i+1 < len(args) && (!strings.HasPrefix(args[i+1], "-") || name == "goal-file" && args[i+1] == "-") {
			i++
			value = args[i]
		}
		if value == "" {
			return o, fmt.Errorf("--%s requires %s", name, flag.value)
		}
		flags[name] = value
	}
	if len(positional) == 0 {
		return o, errors.New("swarm requires roles, bind, unbind, start or stop")
	}
	o.action = positional[0]
	want := map[string]int{"roles": 2, "bind": 4, "unbind": 3, "start": 2, "stop": 2}[o.action]
	if want == 0 {
		return o, errors.New("swarm action must be roles, bind, unbind, start or stop")
	}
	if len(positional) != want {
		return o, errors.New("usage: bro swarm roles SWARM | bind SWARM ROLE ACTOR | unbind SWARM ROLE | start SWARM | stop SWARM; use bro swarm --help")
	}
	for _, name := range []string{"definition", "goal", "goal-file", "commit"} {
		if _, set := flags[name]; set && !slices.Contains(swarmFlags[name].actions, o.action) {
			return o, fmt.Errorf("--%s is not valid for swarm %s", name, o.action)
		}
	}
	o.definition, o.goal, o.goalFile, o.commit = flags["definition"], flags["goal"], flags["goal-file"], flags["commit"]
	if o.action == "start" {
		if o.definition == "" {
			return o, errors.New("swarm start requires --definition NAME")
		}
		if (o.goal == "") == (o.goalFile == "") {
			return o, errors.New("swarm start requires exactly one of --goal TEXT and --goal-file FILE")
		}
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

// swarm — bro swarm: привязки ролей роя к managed-акторам (#235), запуск
// прохода с задачей и остановка (#242).
func (c command) swarm(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		swarmUsage(c.out)
		return nil
	}
	o, err := parseSwarm(args)
	if err != nil {
		return err
	}
	// Задача из файла читается до сети: нечитаемый файл не стоит запросов.
	if o.goalFile != "" {
		if o.goal, err = readGoalFile(o.goalFile, c.in); err != nil {
			return err
		}
	}
	if o.action == "start" || o.action == "stop" {
		return c.swarmExecution(ctx, o)
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

// swarmExecution — bro swarm start и stop: проход роя (#242).
func (c command) swarmExecution(ctx context.Context, o swarmOptions) error {
	client, store, err := newClient("", "")
	if err != nil {
		return err
	}
	swarm, err := resolveSwarm(ctx, client, o.swarm)
	if err != nil {
		return err
	}
	var execution clientapi.SwarmExecution
	verb := "started"
	if o.action == "start" {
		execution, err = client.StartSwarm(ctx, swarm.ID, clientapi.SwarmStart{Definition: o.definition, Goal: o.goal, Commit: o.commit})
	} else {
		verb = "stopped"
		execution, err = client.StopSwarm(ctx, swarm.ID)
	}
	if err != nil {
		return sessionError(err)
	}
	if err := store.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	printSwarmExecution(c.out, verb, swarm, execution)
	return nil
}

// readGoalFile читает задачу прохода из файла или, для «-», из stdin: не
// больше clientapi.MaxSwarmGoalBytes после снятия одного завершающего
// перевода строки, как у значений bro secret. Остальные правила задачи
// проверяет сервер.
func readGoalFile(name string, stdin io.Reader) (string, error) {
	in := stdin
	if name != "-" {
		file, err := os.Open(name)
		if err != nil {
			return "", fmt.Errorf("could not open the goal file %s", name)
		}
		defer file.Close()
		in = file
	}
	data, err := io.ReadAll(io.LimitReader(in, clientapi.MaxSwarmGoalBytes+3))
	if err != nil {
		return "", fmt.Errorf("could not read the goal file %s", name)
	}
	goal := string(data)
	if trimmed, ok := strings.CutSuffix(goal, "\n"); ok {
		goal = strings.TrimSuffix(trimmed, "\r")
	}
	if len(goal) > clientapi.MaxSwarmGoalBytes {
		return "", fmt.Errorf("the goal file exceeds %d bytes", clientapi.MaxSwarmGoalBytes)
	}
	return goal, nil
}

func printSwarmExecution(w io.Writer, verb string, swarm clientapi.Swarm, e clientapi.SwarmExecution) {
	fmt.Fprintf(w, "%s swarm %s (%s): execution %s\n", verb, swarm.ID, swarm.Name, e.Execution)
	if d := e.Definition; d != nil {
		fmt.Fprintf(w, "graph: %s version %d revision %s\n", d.Name, d.Version, d.Revision)
	}
	if src := e.Source; src != nil {
		fmt.Fprintf(w, "source: %s commit %s\n", src.Origin, src.Commit)
	}
	if step := e.Step; step != nil {
		fmt.Fprintf(w, "step %d %s: %s", step.Sequence, step.NodeID, step.State)
		if step.Detail != "" {
			fmt.Fprintf(w, " (%s)", step.Detail)
		}
		fmt.Fprintln(w)
	}
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
