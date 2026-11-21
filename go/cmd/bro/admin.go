// Проекты и пулы: списки и команды администратора.
package main

import (
	"context"
	"errors"
	"fmt"

	"brood.dev/go/brood/clientapi"
)

func (c *command) projects(ctx context.Context, args []string) error {
	flags := newFlags("projects")
	asJSON := flags.Bool("json", false, "")
	if rest, err := parseArgs(flags, args, 0); err != nil || len(rest) > 0 {
		return errors.Join(err, unexpected("projects", rest))
	}
	op, err := c.operator()
	if err != nil {
		return err
	}
	projects, err := op.client.ProjectsList(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(c.out, append([]clientapi.Project{}, projects...))
	}
	if len(projects) == 0 {
		fmt.Fprintln(c.out, "no projects; declare one in brood.yaml and run: bro build")
		return nil
	}
	list := table{header: []string{"NAME", "REPOSITORY", "SWARMS"}}
	for _, project := range projects {
		list.add(toneNone, project.Name, dash(project.Repository), fmt.Sprint(project.SwarmCount))
	}
	return list.write(c.out)
}

func (c *command) pools(ctx context.Context, args []string) error {
	flags := newFlags("pools")
	asJSON := flags.Bool("json", false, "")
	if rest, err := parseArgs(flags, args, 0); err != nil || len(rest) > 0 {
		return errors.Join(err, unexpected("pools", rest))
	}
	op, err := c.operator()
	if err != nil {
		return err
	}
	pools, err := op.client.Pools(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(c.out, append([]clientapi.Pool{}, pools...))
	}
	if len(pools) == 0 {
		fmt.Fprintln(c.out, "no pools; an administrator connects one in the panel")
		return nil
	}
	list := table{header: []string{"NAME", "STATE", "AGENTS", "CPU", "MEMORY"}}
	for _, pool := range pools {
		list.add(poolTone(pool), poolCells(pool)...)
	}
	return list.write(c.out)
}

func poolTone(pool clientapi.Pool) string {
	switch {
	case pool.State == "":
		return toneDim
	case !pool.Available:
		return toneRed
	case pool.State != "enabled":
		return toneYellow
	}
	return toneNone
}

// poolCells — имя, состояние и занятое из предела: агенты, ядра, память.
func poolCells(pool clientapi.Pool) []string {
	if pool.State == "" {
		return []string{pool.Name, "not activated", "-", "-", "-"}
	}
	state := pool.State
	if !pool.Available {
		state += ", unavailable"
	}
	return []string{
		pool.Name, state,
		fmt.Sprintf("%d/%d", pool.UsedAgents, pool.MaxAgents),
		fmt.Sprintf("%s/%s", cores(pool.UsedCPUMillicores), cores(pool.CPUMillicores)),
		fmt.Sprintf("%s/%sG", gibibytes(pool.UsedMemoryBytes), gibibytes(pool.MemoryBytes)),
	}
}

func cores(millicores int64) string {
	return trimZero(fmt.Sprintf("%.1f", float64(millicores)/1000))
}

func gibibytes(bytes int64) string {
	return trimZero(fmt.Sprintf("%.1f", float64(bytes)/(1<<30)))
}

func trimZero(number string) string {
	if len(number) > 2 && number[len(number)-2:] == ".0" {
		return number[:len(number)-2]
	}
	return number
}

// pool — drain и enable: инструменты каталога для администратора.
func (c *command) pool(ctx context.Context, args []string) error {
	if len(args) != 2 || (args[0] != "drain" && args[0] != "enable") || args[1] == "" {
		return errors.New("expected: bro pool drain NAME or bro pool enable NAME")
	}
	action, name := args[0], args[1]
	if _, err := c.invoke(ctx, "pool_"+action, map[string]any{"name": name}); err != nil {
		return named(name, err)
	}
	if action == "drain" {
		fmt.Fprintf(c.out, "draining pool %s: no new agents are placed in it; its agents keep running\n", name)
		return nil
	}
	fmt.Fprintf(c.out, "enabled pool %s\n", name)
	return nil
}
