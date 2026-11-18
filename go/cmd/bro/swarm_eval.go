package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"brood.dev/go/brood/clientapi"
)

const swarmEvalHelp = `usage: bro swarm eval bind SWARM --reference SWARM --pass N --definition NAME --model NAME [--json]
       bro swarm eval start SWARM [--json]
       bro swarm eval status SWARM [--json]

Non-production evaluation has two stages. Create a separate candidate project
for the same repository. The launcher must manage both candidate and reference
scopes. Bind before any candidate actors boot; bind pins the reference pass goal
and commit, candidate definition snapshot and explicit model without starting.
Then configure isolated managed actors and roles in the candidate scope before
using eval start. Credentials and network configuration are not copied.
Start accepts no goal, commit, definition or model overrides and is never retried
automatically. Ordinary swarm start is forbidden for a bound candidate.
Status reads the retained binding and durable applied/stopped replay reason;
it does not submit a historical answer. --json prints the complete API document.
`

type swarmEvalOptions struct {
	action, swarm string
	selection     clientapi.SwarmEvalSelection
	json          bool
}

func parseSwarmEval(args []string) (swarmEvalOptions, error) {
	var o swarmEvalOptions
	fail := func() (swarmEvalOptions, error) { return o, errors.New(swarmEvalHelp) }
	if len(args) < 2 {
		return fail()
	}
	o.action, o.swarm = args[0], args[1]
	valid := func(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.HasPrefix(s, "-") }
	if !valid(o.swarm) || (o.action != "bind" && o.action != "start" && o.action != "status") {
		return fail()
	}
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return fail()
		}
		seen[flag] = true
		if flag == "--json" {
			o.json = true
			continue
		}
		if o.action != "bind" || i+1 >= len(args) {
			return fail()
		}
		i++
		value := args[i]
		if !valid(value) {
			return fail()
		}
		switch flag {
		case "--reference":
			o.selection.ReferenceSwarmID = value
		case "--definition":
			o.selection.Definition = value
		case "--model":
			o.selection.Model = value
		case "--pass":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != value {
				return fail()
			}
			o.selection.ReferencePass = n
		default:
			return fail()
		}
	}
	if o.action == "bind" && (o.selection.ReferenceSwarmID == "" || o.selection.ReferencePass == 0 || o.selection.Definition == "" || o.selection.Model == "") {
		return fail()
	}
	return o, nil
}

func (c command) swarmEval(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(c.out, swarmEvalHelp)
		return err
	}
	o, err := parseSwarmEval(args)
	if err != nil {
		return err
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	candidate, err := resolveSwarm(ctx, client, o.swarm)
	if err != nil {
		return err
	}
	var result any
	if o.action == "bind" {
		reference, err := resolveSwarm(ctx, client, o.selection.ReferenceSwarmID)
		if err != nil {
			return err
		}
		o.selection.ReferenceSwarmID = reference.ID
		result, err = client.BindSwarmEval(ctx, candidate.ID, o.selection)
		if err != nil {
			return sessionError(err)
		}
	} else if o.action == "start" {
		result, err = client.StartSwarmEval(ctx, candidate.ID)
	} else {
		result, err = client.SwarmEval(ctx, candidate.ID)
	}
	if err != nil {
		return sessionError(err)
	}
	if err = sessions.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	if o.json {
		encoder := json.NewEncoder(c.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if execution, ok := result.(clientapi.SwarmExecution); ok {
		var text strings.Builder
		fmt.Fprintf(&text, "non-production eval swarm %q: execution %q\ngoal: %q\n", execution.SwarmID, execution.Execution, execution.Goal)
		if d := execution.Definition; d != nil {
			fmt.Fprintf(&text, "definition: %q version %d revision %q\n", d.Name, d.Version, d.Revision)
		}
		if source := execution.Source; source != nil {
			fmt.Fprintf(&text, "source: %q commit %q\n", source.Origin, source.Commit)
		}
		if step := execution.Step; step != nil {
			fmt.Fprintf(&text, "step %d %q: %q; detail: %q\n", step.Sequence, step.NodeID, step.State, step.Detail)
		}
		_, err = io.WriteString(c.out, text.String())
		return err
	}
	return printSwarmEval(c.out, result.(clientapi.SwarmEval))
}

func printSwarmEval(out io.Writer, status clientapi.SwarmEval) error {
	var text strings.Builder
	fmt.Fprintf(&text, "eval swarm %q: non-production %t; execution %q\nreference: %q pass %d\ndefinition: %q version %d revision %q\nmodel: %q\nsource: %q commit %q\ngoal: %q\n", status.SwarmID, status.NonProduction, status.Execution, status.ReferenceSwarmID, status.ReferencePass, status.Definition.Name, status.Definition.Version, status.Definition.Revision, status.Model, status.Source.Origin, status.Source.Commit, status.Goal)
	if replay := status.Replay; replay != nil {
		fmt.Fprintf(&text, "replay request %q: %q\nreason: %q\ninterrupted children: %t\n", replay.RequestID, replay.State, replay.Reason, replay.InterruptedChildren)
	} else {
		fmt.Fprintln(&text, "replay: none")
	}
	_, err := io.WriteString(out, text.String())
	return err
}
