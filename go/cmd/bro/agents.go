// Команды человека над агентами роя: список, создание и жизненный цикл.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"brood.dev/go/brood/clientapi"
)

// Сервер меняет фазу агента за секунды: чаще опрашивать незачем.
const agentPollInterval = 2 * time.Second

// agentRow — строка списка агентов: состав роя из каталога и размещение.
type agentRow struct {
	Name    string `json:"name"`
	Swarm   string `json:"swarm,omitempty"`
	Role    string `json:"role"`
	State   string `json:"state"`
	Pool    string `json:"pool"`
	CTID    int64  `json:"ctid"`
	Created string `json:"created_at"`
	Note    string `json:"note,omitempty"`
}

// member — агент в ответе инструмента agents.
type member struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	State string `json:"state"`
}

// roster — состав текущего роя по каталогу: то же, что видят агенты и MCP.
func (c *command) roster(ctx context.Context) ([]member, error) {
	result, err := c.invoke(ctx, "agents", map[string]any{})
	if err != nil {
		return nil, err
	}
	var data struct {
		Agents []member `json:"agents"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return nil, errors.New("the server answered agents with unexpected data")
	}
	return data.Agents, nil
}

// role называет роль словом bro: рядовой агент — agent.
func role(value string) string {
	if value == "" {
		return "agent"
	}
	return value
}

func note(agent clientapi.Agent) string {
	if agent.Phase != "error" {
		return ""
	}
	if agent.ErrorMessage != "" {
		return agent.ErrorMessage
	}
	return agent.ErrorCode
}

// swarmRows соединяет состав роя с размещением его агентов.
func (c *command) swarmRows(ctx context.Context, op *operator, swarm clientapi.Swarm) ([]agentRow, error) {
	members, err := c.roster(ctx)
	if err != nil {
		return nil, err
	}
	placed, err := op.client.Agents(ctx, swarm.ID)
	if err != nil {
		return nil, err
	}
	details := map[string]clientapi.Agent{}
	for _, agent := range placed {
		details[agent.Name] = agent
	}
	rows := make([]agentRow, 0, len(members))
	for _, m := range members {
		agent := details[m.Name]
		rows = append(rows, agentRow{Name: m.Name, Role: role(m.Role), State: m.State, Pool: agent.Pool, CTID: agent.CTID, Created: agent.CreatedAt, Note: note(agent)})
	}
	return rows, nil
}

func allRows(agents []clientapi.Agent) []agentRow {
	rows := make([]agentRow, 0, len(agents))
	for _, agent := range agents {
		rows = append(rows, agentRow{Name: agent.Name, Swarm: agent.ProjectName + "/" + agent.SwarmName, Role: role(agent.Role), State: agent.Phase, Pool: agent.Pool, CTID: agent.CTID, Created: agent.CreatedAt, Note: note(agent)})
	}
	return rows
}

// agentsTable — NAME, ROLE, STATE, POOL, CT, AGE; NOTE с причиной — когда
// у кого-то ошибка, SWARM — в списке всех роёв.
func agentsTable(rows []agentRow, withSwarm bool, now time.Time) table {
	list := table{header: []string{"NAME", "ROLE", "STATE", "POOL", "CT", "AGE"}}
	if withSwarm {
		list.header = append([]string{"SWARM"}, list.header...)
	}
	noted := false
	for _, row := range rows {
		noted = noted || row.Note != ""
	}
	if noted {
		list.header = append(list.header, "NOTE")
	}
	for _, row := range rows {
		ct, tone := "-", toneNone
		if row.CTID != 0 {
			ct = strconv.FormatInt(row.CTID, 10)
		}
		switch row.State {
		case "error":
			tone = toneRed
		case "running":
		default:
			tone = toneYellow
		}
		cells := []string{row.Name, row.Role, row.State, dash(row.Pool), ct, age(row.Created, now)}
		if withSwarm {
			cells = append([]string{row.Swarm}, cells...)
		}
		if noted {
			cells = append(cells, brief(row.Note, 60))
		}
		list.add(tone, cells...)
	}
	return list
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func (c *command) agents(ctx context.Context, args []string) error {
	flags := newFlags("agents")
	all := flags.Bool("all", false, "")
	asJSON := flags.Bool("json", false, "")
	if rest, err := parseArgs(flags, args, 0); err != nil || len(rest) > 0 {
		return errors.Join(err, unexpected("agents", rest))
	}
	var rows []agentRow
	if *all {
		op, err := c.operator()
		if err != nil {
			return err
		}
		agents, err := op.client.Agents(ctx, "")
		if err != nil {
			return err
		}
		rows = allRows(agents)
	} else {
		op, swarm, err := c.selected(ctx)
		if err != nil {
			return err
		}
		if rows, err = c.swarmRows(ctx, op, swarm); err != nil {
			return err
		}
	}
	if *asJSON {
		return writeJSON(c.out, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.out, "no agents; create one with: bro add")
		return nil
	}
	list := agentsTable(rows, *all, time.Now())
	return list.write(c.out)
}

func (c *command) status(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return unexpected("status", args)
	}
	op, err := c.operator()
	if err != nil {
		return err
	}
	user, err := op.client.Whoami(ctx)
	if err != nil {
		return err
	}
	who := user.Username
	if user.IsAdmin {
		who += " (administrator)"
	}
	fmt.Fprintf(c.out, "server  %s\nuser    %s\n", op.client.Origin(), who)
	current, err := op.current(ctx)
	var none *noSwarmError
	if errors.As(err, &none) {
		fmt.Fprintf(c.out, "swarm   %s\n", none.Error())
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "swarm   %s (%s)\n\n", label(current.swarm), current.source)
	rows, err := c.swarmRows(ctx, op, current.swarm)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.out, "no agents; create one with: bro add")
		return nil
	}
	list := agentsTable(rows, false, time.Now())
	return list.write(c.out)
}

func (c *command) add(ctx context.Context, args []string) error {
	flags := newFlags("add")
	pool := flags.String("pool", "", "")
	as := flags.String("role", "", "")
	origin := flags.String("origin", "", "")
	noWait := flags.Bool("no-wait", false, "")
	names, err := parseArgs(flags, args, 0)
	if err != nil {
		return err
	}
	if len(names) > 1 {
		return unexpected("add", names[1:])
	}
	op, swarm, err := c.selected(ctx)
	if err != nil {
		return err
	}
	request := clientapi.AgentRequest{Origin: *origin}
	if *as != "" {
		roles, err := op.client.AgentRoles(ctx, swarm.ProjectID)
		if err != nil {
			return err
		}
		available := make([]string, 0, len(roles))
		for _, r := range roles {
			available = append(available, r.Name)
			if r.Name == *as {
				request.Role = r.ID
			}
		}
		if request.Role == "" {
			return fmt.Errorf("choose a project role with --role NAME; available: %s", strings.Join(available, ", "))
		}
	}
	if len(names) == 1 {
		request.Name = names[0]
	} else if request.Name, err = freeName(ctx, op, swarm); err != nil {
		return err
	}
	profiles, err := op.client.PoolProfiles(ctx, swarm.ID)
	if err != nil {
		return err
	}
	if request.Template, err = pickProfile(profiles, *pool); err != nil {
		return err
	}
	agent, err := op.client.AgentCreate(ctx, swarm.ID, request)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "created %s %s in %s (pool %s)\n", role(agent.Role), agent.Name, label(swarm), dash(agent.Pool))
	if *noWait {
		return nil
	}
	return c.awaitAgent(ctx, op, swarm, agent)
}

// freeName — первое свободное имя agent-N в рое.
func freeName(ctx context.Context, op *operator, swarm clientapi.Swarm) (string, error) {
	agents, err := op.client.Agents(ctx, swarm.ID)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, agent := range agents {
		taken[agent.Name] = true
	}
	for n := 1; ; n++ {
		if name := "agent-" + strconv.Itoa(n); !taken[name] {
			return name, nil
		}
	}
}

// pickProfile выбирает профиль пула: названный пул или единственный.
// Профиль называется именем пула с префиксом сервера.
func pickProfile(profiles []string, pool string) (string, error) {
	if len(profiles) == 0 {
		return "", errors.New("no pool accepts new agents; an administrator enables one with: bro pool enable NAME")
	}
	available := strings.Join(profiles, ", ")
	if pool == "" {
		if len(profiles) == 1 {
			return profiles[0], nil
		}
		return "", fmt.Errorf("choose a pool with --pool NAME; available: %s", available)
	}
	var matched []string
	for _, profile := range profiles {
		if profile == pool {
			return profile, nil
		}
		if strings.HasSuffix(profile, "-"+pool) {
			matched = append(matched, profile)
		}
	}
	if len(matched) != 1 {
		return "", fmt.Errorf("pool %s does not name one pool profile; available: %s", pool, available)
	}
	return matched[0], nil
}

// awaitAgent показывает смену фаз одной строкой и ждёт running или error.
func (c *command) awaitAgent(ctx context.Context, op *operator, swarm clientapi.Swarm, agent clientapi.Agent) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	ticker := time.NewTicker(agentPollInterval)
	defer ticker.Stop()
	fmt.Fprintf(c.out, "%s: %s", agent.Name, agent.Phase)
	shown := agent.Phase
	for {
		if agent.Phase != shown {
			fmt.Fprintf(c.out, " -> %s", agent.Phase)
			shown = agent.Phase
		}
		switch agent.Phase {
		case "running":
			fmt.Fprintln(c.out)
			return nil
		case "error":
			fmt.Fprintln(c.out)
			return fmt.Errorf("agent %s failed: %s", agent.Name, dash(brief(note(agent), 200)))
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(c.out)
			return fmt.Errorf("stopped waiting; agent %s is still being created (see: bro agents)", agent.Name)
		case <-ticker.C:
		}
		current, err := op.client.Agent(ctx, swarm.ID, agent.Name)
		switch {
		case err == nil:
			agent = current
		case ctx.Err() != nil:
			// Прерывание посреди запроса: отчёт даст следующий оборот цикла.
		default:
			fmt.Fprintln(c.out)
			return err
		}
	}
}

// destroy — rm и reset: необратимые команды спрашивают согласие.
func (c *command) destroy(ctx context.Context, action string, args []string) error {
	flags := newFlags(action)
	yes := flags.Bool("yes", false, "")
	names, err := parseArgs(flags, args, 0)
	if err != nil {
		return err
	}
	question, done := "Delete agent %s?", "deleting"
	if action == "reset" {
		question, done = "Reset agent %s? Its workspace is destroyed.", "resetting"
	}
	if len(names) == 0 || (action == "reset" && len(names) != 1) {
		return fmt.Errorf("expected an agent name; see: bro %s --help", action)
	}
	for _, name := range names {
		if err := c.confirm(fmt.Sprintf(question, name), *yes); err != nil {
			return err
		}
		if _, err := c.invoke(ctx, action, map[string]any{"name": name, "confirm": true}); err != nil {
			return named(name, err)
		}
		fmt.Fprintf(c.out, "%s %s\n", done, name)
	}
	return nil
}

// named ставит имя агента перед причиной отказа.
func named(name string, err error) error {
	if errors.Is(err, clientapi.ErrSessionExpired) || errors.Is(err, errNotLoggedIn) {
		return err
	}
	return fmt.Errorf("%s: %s", name, describe(err))
}

// lifecycle — start, stop и restart: без подтверждения, рабочая копия цела.
func (c *command) lifecycle(ctx context.Context, action string, args []string) error {
	names, err := parseArgs(newFlags(action), args, 0)
	if err != nil {
		return err
	}
	if len(names) == 0 || (action == "restart" && len(names) != 1) {
		return fmt.Errorf("expected an agent name; see: bro %s --help", action)
	}
	done := map[string]string{"start": "starting", "stop": "stopping", "restart": "restarted the session of"}[action]
	for _, name := range names {
		if _, err := c.invoke(ctx, action, map[string]any{"name": name}); err != nil {
			return named(name, err)
		}
		fmt.Fprintf(c.out, "%s %s\n", done, name)
	}
	return nil
}
