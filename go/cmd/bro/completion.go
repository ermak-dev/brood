// Дополнение команд в оболочке: скрипт и скрытый источник имён.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Оболочка ждёт дополнение на каждом Tab: дольше ждать нельзя.
const completeTimeout = 2 * time.Second

// commandWords — первые слова команд и вторые слова составных.
func commandWords() (first []string, second map[string][]string) {
	second = map[string][]string{}
	seen := map[string]bool{}
	for _, t := range topics() {
		head, tail, compound := strings.Cut(t.name, " ")
		if !seen[head] {
			seen[head] = true
			first = append(first, head)
		}
		if compound {
			second[head] = append(second[head], tail)
		}
	}
	return append(first, "help"), second
}

func (c *command) completion(args []string) error {
	if len(args) != 1 || (args[0] != "bash" && args[0] != "zsh") {
		return errors.New("expected: bro completion bash or bro completion zsh")
	}
	first, second := commandWords()
	var cases strings.Builder
	heads := make([]string, 0, len(second))
	for head := range second {
		heads = append(heads, head)
	}
	sort.Strings(heads)
	for _, head := range heads {
		fmt.Fprintf(&cases, "      %s) words=%q ;;\n", head, strings.Join(second[head], " "))
	}
	script := strings.NewReplacer("@COMMANDS@", strings.Join(first, " "), "@SECOND@", strings.TrimRight(cases.String(), "\n")).Replace(completionScript)
	if args[0] == "zsh" {
		script = "autoload -U +X bashcompinit && bashcompinit\n" + script
	}
	_, err := io.WriteString(c.out, script)
	return err
}

// completionScript — дополнение bash; zsh исполняет его через bashcompinit.
// Имена роёв и агентов даёт bro __complete: он молчит при любой ошибке.
const completionScript = `_bro() {
  local cur prev command words
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  command="${COMP_WORDS[1]}"
  if [[ "$prev" == "-s" || "$prev" == "--swarm" ]]; then
    words="$(bro __complete swarms 2>/dev/null)"
  elif [[ $COMP_CWORD -eq 1 ]]; then
    words="@COMMANDS@"
  elif [[ $COMP_CWORD -eq 2 && "$command" == "swarm" ]]; then
    words="add rm"
  elif [[ "$command" == "join" || ( "$command" == "swarm" && "${COMP_WORDS[2]}" == "rm" ) ]]; then
    words="$(bro __complete swarms 2>/dev/null)"
  elif [[ $COMP_CWORD -eq 2 ]]; then
    case "$command" in
@SECOND@
      completion) words="bash zsh" ;;
      rm|start|stop|restart|reset|attach|send|tail|slash) words="$(bro __complete agents 2>/dev/null)" ;;
    esac
  else
    case "$command" in
      rm|start|stop) words="$(bro __complete agents 2>/dev/null)" ;;
    esac
  fi
  COMPREPLY=($(compgen -W "$words" -- "$cur"))
}
complete -F _bro bro
`

// complete печатает имена для дополнения по одному в строке. Любая ошибка —
// пустой вывод: дополнение не спрашивает и не ждёт дольше completeTimeout.
func (c *command) complete(ctx context.Context, args []string) {
	if len(args) != 1 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	op, err := c.operator()
	if err != nil {
		return
	}
	// Дополнение ничего не пишет: ни заметок, ни сессии.
	op.notice, c.session = io.Discard, nil
	var names []string
	switch args[0] {
	case "swarms":
		names, err = completeSwarms(ctx, op)
	case "agents":
		names, err = completeAgents(ctx, op)
	}
	if err != nil {
		return
	}
	for _, name := range names {
		fmt.Fprintln(c.out, name)
	}
}

// completeSwarms — имена роёв; одноимённые — в виде project/swarm.
func completeSwarms(ctx context.Context, op *operator) ([]string, error) {
	swarms, err := op.client.Swarms(ctx)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, swarm := range swarms {
		count[swarm.Name]++
	}
	names := make([]string, 0, len(swarms))
	for _, swarm := range swarms {
		if count[swarm.Name] > 1 {
			names = append(names, label(swarm))
		} else {
			names = append(names, swarm.Name)
		}
	}
	return names, nil
}

func completeAgents(ctx context.Context, op *operator) ([]string, error) {
	current, err := op.current(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := op.client.Agents(ctx, current.swarm.ID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		names = append(names, agent.Name)
	}
	return names, nil
}
