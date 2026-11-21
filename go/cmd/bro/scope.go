// Оператор и его рой: сессия сервера, выбор роя и команды над роями.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/clientapi"
)

// Откуда взят текущий рой.
const (
	sourceFlag   = "--swarm"
	sourceEnv    = "BROOD_SWARM"
	sourceJoined = "joined"
	sourceOnly   = "the only swarm"
)

// operator — вошедший пользователь на выбранном сервере.
type operator struct {
	client  *clientapi.Client
	store   *clientapi.SessionStore
	scope   scope
	notice  io.Writer
	initial clientapi.StoredSession
}

// operator открывает сохранённую сессию один раз за запуск.
func (c *command) operator() (*operator, error) {
	if c.session != nil {
		return c.session, nil
	}
	client, store, err := newClient("", "")
	if err != nil {
		return nil, err
	}
	if client.Stored().Session == "" {
		return nil, errNotLoggedIn
	}
	c.session = &operator{client: client, store: store, scope: c.scope, notice: c.diagnostic, initial: client.Stored()}
	return c.session, nil
}

// save сохраняет cookie, если сервер их сменил.
func (o *operator) save() error {
	if stored := o.client.Stored(); stored != o.initial && !stored.Empty() {
		return o.store.Save(o.client.Origin(), stored)
	}
	return nil
}

// choice — текущий рой и источник выбора.
type choice struct {
	swarm  clientapi.Swarm
	source string
}

// noSwarmError — рой не выбран и выбрать его за человека нельзя.
type noSwarmError struct{ available []string }

func (e *noSwarmError) Error() string {
	if len(e.available) == 0 {
		return "no swarms yet; create one with: bro swarm add NAME"
	}
	return "no swarm selected; available: " + strings.Join(e.available, ", ") + "; choose one with: bro join NAME"
}

func label(swarm clientapi.Swarm) string { return swarm.ProjectName + "/" + swarm.Name }

func labels(swarms []clientapi.Swarm) []string {
	names := make([]string, 0, len(swarms))
	for _, swarm := range swarms {
		names = append(names, label(swarm))
	}
	sort.Strings(names)
	return names
}

// matchSwarm находит рой по точному ID, затем по project/swarm, затем по
// имени, если оно одно.
func matchSwarm(swarms []clientapi.Swarm, value string) (clientapi.Swarm, error) {
	for _, swarm := range swarms {
		if swarm.ID == value {
			return swarm, nil
		}
	}
	for _, swarm := range swarms {
		if label(swarm) == value {
			return swarm, nil
		}
	}
	var named []clientapi.Swarm
	for _, swarm := range swarms {
		if swarm.Name == value {
			named = append(named, swarm)
		}
	}
	switch {
	case len(named) == 1:
		return named[0], nil
	case len(named) > 1:
		return clientapi.Swarm{}, fmt.Errorf("swarm name %s is ambiguous; use one of: %s", value, strings.Join(labels(named), ", "))
	case len(swarms) == 0:
		return clientapi.Swarm{}, fmt.Errorf("swarm %s not found; you have no swarms yet", value)
	}
	return clientapi.Swarm{}, fmt.Errorf("swarm %s not found; available: %s", value, strings.Join(labels(swarms), ", "))
}

// current выбирает рой: --swarm, BROOD_SWARM, выбранный bro join,
// единственный доступный; иначе noSwarmError.
func (o *operator) current(ctx context.Context) (choice, error) {
	swarms, err := o.client.Swarms(ctx)
	if err != nil {
		return choice{}, err
	}
	return o.choose(swarms)
}

func (o *operator) choose(swarms []clientapi.Swarm) (choice, error) {
	for _, named := range []struct{ value, source string }{{o.scope.flag, sourceFlag}, {o.scope.env, sourceEnv}} {
		if named.value != "" {
			swarm, err := matchSwarm(swarms, named.value)
			return choice{swarm, named.source}, err
		}
	}
	joined, err := o.store.Swarm(o.client.Origin())
	if err != nil {
		return choice{}, err
	}
	for _, swarm := range swarms {
		if joined != "" && swarm.ID == joined {
			return choice{swarm, sourceJoined}, nil
		}
	}
	if joined != "" {
		if err := o.store.SetSwarm(o.client.Origin(), ""); err != nil {
			return choice{}, err
		}
		fmt.Fprintln(o.notice, "bro: the joined swarm no longer exists; forgotten")
	}
	if len(swarms) == 1 {
		return choice{swarms[0], sourceOnly}, nil
	}
	return choice{}, &noSwarmError{labels(swarms)}
}

// selected — оператор и его текущий рой: начало каждой команды роя.
func (c *command) selected(ctx context.Context) (*operator, clientapi.Swarm, error) {
	op, err := c.operator()
	if err != nil {
		return nil, clientapi.Swarm{}, err
	}
	current, err := op.current(ctx)
	return op, current.swarm, err
}

func (c *command) join(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return errors.New("join accepts one swarm; see: bro join --help")
	}
	op, err := c.operator()
	if err != nil {
		return err
	}
	swarms, err := op.client.Swarms(ctx)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		current, err := op.choose(swarms)
		var none *noSwarmError
		if errors.As(err, &none) {
			fmt.Fprintln(c.out, none.Error())
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s (%s)\n", label(current.swarm), current.source)
		return nil
	}
	swarm, err := matchSwarm(swarms, args[0])
	if err != nil {
		return err
	}
	if err := op.store.SetSwarm(op.client.Origin(), swarm.ID); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "joined %s\n", label(swarm))
	if op.scope.flag != "" || op.scope.env != "" {
		fmt.Fprintln(c.diagnostic, "bro: --swarm and BROOD_SWARM still take precedence over the joined swarm")
	}
	return nil
}

func (c *command) swarms(ctx context.Context, args []string) error {
	flags := newFlags("swarms")
	asJSON := flags.Bool("json", false, "")
	if rest, err := parseArgs(flags, args, 0); err != nil || len(rest) > 0 {
		return errors.Join(err, unexpected("swarms", rest))
	}
	op, err := c.operator()
	if err != nil {
		return err
	}
	swarms, err := op.client.Swarms(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(c.out, append([]clientapi.Swarm{}, swarms...))
	}
	if len(swarms) == 0 {
		fmt.Fprintln(c.out, "no swarms; create one with: bro swarm add NAME")
		return nil
	}
	op.notice = io.Discard
	current, _ := op.choose(swarms)
	sort.Slice(swarms, func(i, j int) bool { return label(swarms[i]) < label(swarms[j]) })
	list := table{header: []string{"  NAME", "PROJECT", "OWNER", "AGENTS", "CREATED"}}
	now := time.Now()
	for _, swarm := range swarms {
		mark := "  "
		if swarm.ID == current.swarm.ID {
			mark = "* "
		}
		list.add(toneNone, mark+swarm.Name, swarm.ProjectName, swarm.Owner, fmt.Sprint(swarm.AgentCount), age(swarm.CreatedAt, now))
	}
	return list.write(c.out)
}

func unexpected(name string, rest []string) error {
	if len(rest) == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %s; see: bro %s --help", rest[0], name)
}

func (c *command) swarm(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "add" && args[0] != "rm") {
		return errors.New("expected: bro swarm add NAME or bro swarm rm NAME")
	}
	flags := newFlags("swarm " + args[0])
	project := flags.String("project", "", "")
	yes := flags.Bool("yes", false, "")
	names, err := parseArgs(flags, args[1:], 0)
	if err != nil {
		return err
	}
	if len(names) != 1 || names[0] == "" {
		return fmt.Errorf("expected one swarm name; see: bro swarm %s --help", args[0])
	}
	op, err := c.operator()
	if err != nil {
		return err
	}
	if args[0] == "add" {
		return c.swarmAdd(ctx, op, names[0], *project)
	}
	return c.swarmRemove(ctx, op, names[0], *yes)
}

func (c *command) swarmAdd(ctx context.Context, op *operator, name, projectName string) error {
	projects, err := op.client.ProjectsList(ctx)
	if err != nil {
		return err
	}
	project, err := defaultProject(projects, projectName)
	if err != nil {
		return err
	}
	swarm, err := op.client.SwarmCreate(ctx, project.ID, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "created swarm %s/%s\n", project.Name, swarm.Name)
	joined, err := op.store.Swarm(op.client.Origin())
	if err != nil || joined != "" {
		return err
	}
	if err := op.store.SetSwarm(op.client.Origin(), swarm.ID); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "joined %s/%s\n", project.Name, swarm.Name)
	return nil
}

// defaultProject выбирает проект нового роя: названный, единственный или
// brood.project из ./brood.yaml.
func defaultProject(projects []clientapi.Project, name string) (clientapi.Project, error) {
	if name == "" && len(projects) == 1 {
		return projects[0], nil
	}
	if name == "" {
		if _, err := os.Stat(build.Filename); err == nil {
			declared, err := build.Load(".")
			if err != nil {
				return clientapi.Project{}, err
			}
			name = declared.Name
		}
	}
	var names []string
	for _, project := range projects {
		if project.Name == name || project.ID == name {
			return project, nil
		}
		names = append(names, project.Name)
	}
	sort.Strings(names)
	switch {
	case len(names) == 0:
		return clientapi.Project{}, errors.New("no projects; declare one in brood.yaml and run: bro build")
	case name == "":
		return clientapi.Project{}, fmt.Errorf("choose a project with --project NAME; available: %s", strings.Join(names, ", "))
	}
	return clientapi.Project{}, fmt.Errorf("project %s not found; available: %s", name, strings.Join(names, ", "))
}

func (c *command) swarmRemove(ctx context.Context, op *operator, name string, yes bool) error {
	swarms, err := op.client.Swarms(ctx)
	if err != nil {
		return err
	}
	swarm, err := matchSwarm(swarms, name)
	if err != nil {
		return err
	}
	if err := c.confirm("Delete swarm "+label(swarm)+"?", yes); err != nil {
		return err
	}
	if err := op.client.SwarmDelete(ctx, swarm.ID); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "deleted swarm %s\n", label(swarm))
	if joined, err := op.store.Swarm(op.client.Origin()); err != nil || joined != swarm.ID {
		return err
	}
	return op.store.SetSwarm(op.client.Origin(), "")
}
