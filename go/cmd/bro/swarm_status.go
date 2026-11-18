package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"brood.dev/go/brood/clientapi"
)

const swarmStatusHelp = `usage: bro swarm status SWARM [--json]

Operator-only, read-only durable swarm execution. SWARM is an accessible swarm
ID or unique name. Reads the same execution view as the panel, without starting,
stopping, answering or resending work. An unpinned swarm has no execution view.
Status uses the existing wire states, including waiting_human. Visits are retained
history; source commit is the pass-start source, not workspace attestation.
Missing legacy source/definition data remains unknown. --json prints typed wire
fields; it does not invent a separate run, source object or pass endpoint.
`

func (c command) swarmStatus(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(c.out, swarmStatusHelp)
		return err
	}
	var selected string
	var asJSON bool
	for _, arg := range args {
		switch {
		case arg == "--json" && !asJSON:
			asJSON = true
		case strings.HasPrefix(arg, "-") || selected != "" || arg == "" || len(arg) > 256:
			return errors.New("usage: bro swarm status SWARM [--json]")
		default:
			selected = arg
		}
	}
	if selected == "" {
		return errors.New("usage: bro swarm status SWARM [--json]")
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	swarm, err := resolveSwarm(ctx, client, selected)
	if err != nil {
		return sessionError(err)
	}
	view, err := client.SwarmStatus(ctx, swarm.ID)
	if err != nil {
		return sessionError(err)
	}
	if err := sessions.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	if asJSON {
		encoder := json.NewEncoder(c.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(view)
	}
	return printSwarmStatus(c.out, view)
}

func printSwarmStatus(out io.Writer, view clientapi.SwarmStatus) error {
	var text strings.Builder
	fmt.Fprintf(&text, "swarm %q (%q): %s\n", view.SwarmID, view.SwarmName, view.Status)
	if d := view.Definition; d != nil {
		fmt.Fprintf(&text, "graph: %q version %d revision %q\n", d.Name, d.Version, d.Revision)
	} else {
		fmt.Fprintln(&text, "graph identity: unknown")
	}
	fmt.Fprintf(&text, "goal: %q\ncurrent node: %q\n", view.Goal, view.CurrentNode)
	source := ""
	for _, node := range view.Nodes {
		// Не подставляем source предыдущего прохода в legacy-проход без него.
		if node.Pass == node.Sequence || node.Goal != "" {
			source = node.SourceCommit
		}
	}
	if source == "" {
		fmt.Fprintln(&text, "source commit: unknown")
	} else {
		fmt.Fprintf(&text, "source commit: %q\n", source)
	}
	if len(view.Nodes) == 0 {
		fmt.Fprintln(&text, "no retained visits")
	}
	for _, node := range view.Nodes {
		fmt.Fprintf(&text, "visit %d %q: %s (pass %d)\n", node.Sequence, node.ID, node.Status, node.Pass)
		for _, field := range []struct{ label, value string }{
			{"actor", node.ActorName}, {"outcome", node.Outcome}, {"goal", node.Goal}, {"source commit", node.SourceCommit},
			{"attested commit", node.Commit}, {"detail", node.Detail}, {"report", node.Report},
		} {
			if field.value != "" {
				fmt.Fprintf(&text, "  %s: %q\n", field.label, field.value)
			}
		}
	}
	if gate := view.Gate; gate != nil {
		fmt.Fprintf(&text, "gate: %q\n", gate.Question)
		for _, option := range gate.Options {
			fmt.Fprintf(&text, "  %q: %q\n", option.ID, option.Label)
		}
	}
	_, err := io.WriteString(out, text.String())
	return err
}
