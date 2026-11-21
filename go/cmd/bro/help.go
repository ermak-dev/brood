// Справка bro: короткий обзор по группам и страница каждой команды.
package main

import (
	"fmt"
	"io"
	"strings"
)

// topic — команда в справке: строка обзора и её страница.
type topic struct {
	group, name, synopsis, summary string
	usage                          string
	about                          string
	args                           [][2]string
	example                        string
}

const (
	groupSession = "Session"
	groupSwarms  = "Swarms"
	groupAgents  = "Agents"
	groupWork    = "Working with an agent"
	groupProject = "Project"
	groupAdmin   = "Administrator"
)

func groups() []string {
	return []string{groupSession, groupSwarms, groupAgents, groupWork, groupProject, groupAdmin}
}

var (
	argSwarm = [2]string{"-s, --swarm NAME", "act in this swarm instead of the current one"}
	argJSON  = [2]string{"--json", "print the list as JSON"}
	argYes   = [2]string{"--yes", "do not ask for confirmation"}
)

func topics() []topic {
	return []topic{
		{group: groupSession, name: "login", synopsis: "login [SERVER]", summary: "log in and remember the server",
			usage: "bro login [SERVER] [--ca FILE]",
			about: "Asks for the user name and the password and stores the session for the\nserver. Passwords are never saved. Without SERVER, the last server is used.",
			args: [][2]string{
				{"SERVER", "server address; HTTPS unless it is localhost"},
				{"--ca FILE", "trust this PEM CA for the server and remember it"},
			},
			example: "bro login brood.example.com"},
		{group: groupSession, name: "logout", synopsis: "logout", summary: "end the session on the server and forget it",
			usage:   "bro logout [SERVER]",
			args:    [][2]string{{"SERVER", "server to log out from (default: the current one)"}},
			example: "bro logout"},
		{group: groupSession, name: "status", synopsis: "status", summary: "show the server, the user, the current swarm and its agents",
			usage:   "bro status [-s SWARM]",
			args:    [][2]string{argSwarm},
			example: "bro status"},
		{group: groupSession, name: "diagnose", synopsis: "diagnose", summary: "show which server and session bro uses, without the network",
			usage:   "bro diagnose",
			example: "bro diagnose"},
		{group: groupSession, name: "completion", synopsis: "completion bash|zsh", summary: "print a shell completion script",
			usage:   "bro completion bash|zsh",
			about:   "The script completes commands, swarm names and agent names.",
			args:    [][2]string{{"bash|zsh", "the shell to complete in"}},
			example: "source <(bro completion bash)"},

		{group: groupSwarms, name: "swarms", synopsis: "swarms", summary: "list the swarms you can use; * marks the current one",
			usage:   "bro swarms [--json]",
			args:    [][2]string{argJSON},
			example: "bro swarms"},
		{group: groupSwarms, name: "join", synopsis: "join [SWARM]", summary: "choose the current swarm, or show it",
			usage: "bro join [SWARM]",
			about: "The chosen swarm is remembered for the server until you join another.",
			args: [][2]string{
				{"SWARM", "swarm name, project/swarm or ID; without it the current swarm is shown"},
			},
			example: "bro join brood/main"},
		{group: groupSwarms, name: "swarm add", synopsis: "swarm add NAME", summary: "create a swarm",
			usage: "bro swarm add NAME [--project NAME]",
			about: "You become the owner. The new swarm becomes the current one when none is joined.",
			args: [][2]string{
				{"NAME", "name of the new swarm"},
				{"--project NAME", "project of the swarm (default: the only project, else brood.project of ./brood.yaml)"},
			},
			example: "bro swarm add review --project brood"},
		{group: groupSwarms, name: "swarm rm", synopsis: "swarm rm NAME", summary: "delete a swarm that has no agents",
			usage: "bro swarm rm NAME [--yes]",
			args: [][2]string{
				{"NAME", "swarm name, project/swarm or ID"},
				argYes,
			},
			example: "bro swarm rm brood/review"},

		{group: groupAgents, name: "agents", synopsis: "agents", summary: "list the agents of the swarm",
			usage: "bro agents [--all] [--json] [-s SWARM]",
			args: [][2]string{
				{"--all", "list the agents of every swarm you can use"},
				argJSON,
				argSwarm,
			},
			example: "bro agents --all"},
		{group: groupAgents, name: "add", synopsis: "add [NAME]", summary: "create an agent and wait until it runs",
			usage: "bro add [NAME] [--pool NAME] [--role agent|master] [--origin URL] [--no-wait] [-s SWARM]",
			about: "The agent gets its own container and a workspace cloned from the origin.",
			args: [][2]string{
				{"NAME", "agent name (default: the next free agent-N)"},
				{"--pool NAME", "pool to place the agent in (default: the only pool)"},
				{"--role agent|master", "master may create and direct other agents (default: agent)"},
				{"--origin URL", "HTTPS Git repository to clone (default: the project repository)"},
				{"--no-wait", "return at once instead of waiting for the agent to run"},
				argSwarm,
			},
			example: "bro add reviewer --pool brood"},
		{group: groupAgents, name: "rm", synopsis: "rm NAME...", summary: "delete agents with their containers",
			usage:   "bro rm NAME... [--yes] [-s SWARM]",
			args:    [][2]string{{"NAME...", "agents to delete"}, argYes, argSwarm},
			example: "bro rm agent-3 agent-4"},
		{group: groupAgents, name: "start", synopsis: "start NAME...", summary: "start stopped agents; the workspace is kept",
			usage:   "bro start NAME... [-s SWARM]",
			args:    [][2]string{{"NAME...", "agents to start"}, argSwarm},
			example: "bro start agent-1"},
		{group: groupAgents, name: "stop", synopsis: "stop NAME...", summary: "stop agents; containers and workspaces are kept",
			usage:   "bro stop NAME... [-s SWARM]",
			args:    [][2]string{{"NAME...", "agents to stop"}, argSwarm},
			example: "bro stop agent-1 agent-2"},
		{group: groupAgents, name: "restart", synopsis: "restart NAME", summary: "restart the agent's session in the same container",
			usage:   "bro restart NAME [-s SWARM]",
			args:    [][2]string{{"NAME", "agent to restart"}, argSwarm},
			example: "bro restart agent-1"},
		{group: groupAgents, name: "reset", synopsis: "reset NAME", summary: "replace the container with a fresh one; the workspace is lost",
			usage:   "bro reset NAME [--yes] [-s SWARM]",
			args:    [][2]string{{"NAME", "agent to reset"}, argYes, argSwarm},
			example: "bro reset agent-1"},

		{group: groupWork, name: "attach", synopsis: "attach NAME", summary: "open the agent's console in this terminal; Ctrl-] detaches",
			usage: "bro attach NAME [--view] [-s SWARM]",
			about: "Everything you type goes to the agent, Ctrl-C and Ctrl-D included.\nCtrl-] detaches and leaves the agent running.",
			args: [][2]string{
				{"NAME", "a running agent"},
				{"--view", "watch the screen without typing: plain text, refreshed every 2 seconds"},
				argSwarm,
			},
			example: "bro attach agent-1"},
		{group: groupWork, name: "send", synopsis: "send NAME MESSAGE...", summary: "send a message to an agent",
			usage: "bro send [--wait SECONDS] [-s SWARM] NAME MESSAGE...",
			about: "Reports whether the message was delivered, not the agent's answer.\nOptions stand before the message; the message is taken as typed.",
			args: [][2]string{
				{"NAME", "agent to write to"},
				{"MESSAGE...", "the message; a single - reads it from stdin"},
				{"--wait SECONDS", "wait up to so many seconds for the agent to become idle (0-300)"},
				argSwarm,
			},
			example: "bro send agent-1 run the tests and report"},
		{group: groupWork, name: "tail", synopsis: "tail NAME", summary: "show the recent output of an agent",
			usage: "bro tail NAME [-n LINES] [-f] [--grep TEXT] [-s SWARM]",
			args: [][2]string{
				{"NAME", "agent to read"},
				{"-n LINES", "number of lines (default: 40)"},
				{"-f", "keep printing new lines until Ctrl-C"},
				{"--grep TEXT", "only the lines that contain TEXT"},
				argSwarm,
			},
			example: "bro tail agent-1 -n 100 -f"},
		{group: groupWork, name: "slash", synopsis: "slash NAME COMMAND", summary: "send a slash command to the agent's session",
			usage:   "bro slash NAME COMMAND [-s SWARM]",
			args:    [][2]string{{"NAME", "agent to command"}, {"COMMAND", "slash command, quoted when it has arguments"}, argSwarm},
			example: "bro slash agent-1 /clear"},
		{group: groupWork, name: "mcp", synopsis: "mcp", summary: "serve these commands to a model as MCP tools over stdio",
			usage:   "bro mcp [-s SWARM]",
			about:   "The tools act in the current swarm, chosen anew for every call.",
			example: "claude mcp add brood -- bro mcp"},

		{group: groupProject, name: "projects", synopsis: "projects", summary: "list the projects you can use",
			usage:   "bro projects [--json]",
			args:    [][2]string{argJSON},
			example: "bro projects"},
		{group: groupProject, name: "build", synopsis: "build", summary: "declare the project from ./brood.yaml and build its agent image"},
		{group: groupProject, name: "image list", synopsis: "image list", summary: "list the agent images of the project"},
		{group: groupProject, name: "image rm", synopsis: "image rm IMAGE", summary: "remove an agent image"},
		{group: groupProject, name: "secret", synopsis: "secret ...", summary: "manage the project's secrets; see bro secret --help"},

		{group: groupAdmin, name: "pools", synopsis: "pools", summary: "list the pools with their state and use",
			usage:   "bro pools [--json]",
			about:   "AGENTS, CPU (cores) and MEMORY show what is used out of the limit.",
			args:    [][2]string{argJSON},
			example: "bro pools"},
		{group: groupAdmin, name: "pool drain", synopsis: "pool drain NAME", summary: "stop placing new agents in a pool",
			usage:   "bro pool drain NAME",
			about:   "Agents already in the pool keep running.",
			args:    [][2]string{{"NAME", "pool to drain"}},
			example: "bro pool drain brood"},
		{group: groupAdmin, name: "pool enable", synopsis: "pool enable NAME", summary: "place new agents in a drained pool again",
			usage:   "bro pool enable NAME",
			args:    [][2]string{{"NAME", "pool to enable"}},
			example: "bro pool enable brood"},
		{group: groupAdmin, name: "server diagnose", synopsis: "server diagnose", summary: "check the server's database and agent relay",
			usage:   "bro server diagnose",
			example: "bro server diagnose"},
	}
}

// findTopic находит команду по началу аргументов: сначала из двух слов.
func findTopic(args []string) (topic, bool) {
	for words := min(2, len(args)); words > 0; words-- {
		for _, t := range topics() {
			if t.name == strings.Join(args[:words], " ") {
				return t, true
			}
		}
	}
	return topic{}, false
}

// wantsHelp сообщает, что команду просят о справке. В send после имени
// агента идёт сообщение, и -h в нём — слово человека.
func wantsHelp(name string, rest []string) bool {
	for i, arg := range rest {
		if arg == "--" || (name == "send" && i > 0) {
			return false
		}
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func overview(w io.Writer) {
	fmt.Fprintln(w, "usage: bro COMMAND [ARGUMENTS]")
	for _, group := range groups() {
		fmt.Fprintf(w, "\n%s\n", group)
		for _, t := range topics() {
			if t.group == group {
				fmt.Fprintf(w, "  %-22s %s\n", t.synopsis, t.summary)
			}
		}
	}
	fmt.Fprint(w, `
Commands on agents act in the current swarm: the one chosen with bro join, or your only swarm.
-s NAME or BROOD_SWARM names another one. Run bro COMMAND --help for arguments and an example.
`)
}

// help печатает обзор или страницу команды.
func (c *command) help(args []string) error {
	var words []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			words = append(words, arg)
		}
	}
	if len(words) == 0 {
		overview(c.out)
		return nil
	}
	switch words[0] {
	case "build":
		buildUsage(c.out)
		return nil
	case "image":
		imageUsage(c.out)
		return nil
	case "secret":
		secretUsage(c.out)
		return nil
	}
	t, found := findTopic(words)
	if !found {
		return fmt.Errorf("unknown command %s; see: bro help", words[0])
	}
	page(c.out, t)
	return nil
}

func page(w io.Writer, t topic) {
	fmt.Fprintf(w, "usage: %s\n\n%s.\n", t.usage, strings.ToUpper(t.summary[:1])+t.summary[1:])
	if t.about != "" {
		fmt.Fprintln(w, t.about)
	}
	if len(t.args) > 0 {
		fmt.Fprintln(w)
	}
	for _, arg := range t.args {
		fmt.Fprintf(w, "  %-20s %s\n", arg[0], arg[1])
	}
	fmt.Fprintf(w, "\nexample: %s\n", t.example)
}
