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

type swarmComparisonOptions struct {
	reference, candidate         string
	referencePass, candidatePass int64
	json                         bool
}

func parseSwarmComparison(args []string) (swarmComparisonOptions, error) {
	var result swarmComparisonOptions
	var positional []string
	for _, arg := range args {
		if arg == "--json" && !result.json {
			result.json = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return result, errors.New("usage: bro swarm compare SWARM PASS OTHER_SWARM OTHER_PASS [--json]")
		}
		positional = append(positional, arg)
	}
	if len(positional) != 4 || positional[0] == "" || positional[2] == "" || len(positional[0]) > 256 || len(positional[2]) > 256 {
		return result, errors.New("usage: bro swarm compare SWARM PASS OTHER_SWARM OTHER_PASS [--json]")
	}
	left, err := strconv.ParseInt(positional[1], 10, 64)
	if err != nil || left < 1 || strconv.FormatInt(left, 10) != positional[1] {
		return result, errors.New("comparison pass numbers must be canonical positive integers")
	}
	right, err := strconv.ParseInt(positional[3], 10, 64)
	if err != nil || right < 1 || strconv.FormatInt(right, 10) != positional[3] {
		return result, errors.New("comparison pass numbers must be canonical positive integers")
	}
	result.reference, result.candidate = positional[0], positional[2]
	result.referencePass, result.candidatePass = left, right
	return result, nil
}

func (c command) swarmCompare(ctx context.Context, args []string) error {
	options, err := parseSwarmComparison(args)
	if err != nil {
		return err
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	reference, err := resolveSwarm(ctx, client, options.reference)
	if err != nil {
		return sessionError(err)
	}
	candidate := reference
	if options.candidate != options.reference {
		candidate, err = resolveSwarm(ctx, client, options.candidate)
		if err != nil {
			return sessionError(err)
		}
	}
	comparison, err := client.CompareSwarmPasses(ctx, reference.ID, options.referencePass, candidate.ID, options.candidatePass)
	if err != nil {
		return sessionError(err)
	}
	if err := sessions.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	if options.json {
		encoder := json.NewEncoder(c.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(comparison)
	}
	return printSwarmComparison(c.out, comparison)
}

func printSwarmComparison(out io.Writer, comparison clientapi.SwarmPassComparison) error {
	var text strings.Builder
	fmt.Fprintf(&text, "reference: %q pass %d\ncandidate: %q pass %d\nalignment: %s (not semantic equivalence)\n", comparison.Reference.SwarmID, comparison.Reference.Pass, comparison.Candidate.SwarmID, comparison.Candidate.Pass, comparison.Alignment)
	yesNo := func(value bool) string {
		if value {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(&text, "same goal: %s\nsame source: %s\nsame definition: %s\n", yesNo(comparison.SameGoal), yesNo(comparison.SameSource), yesNo(comparison.SameDefinition))
	fmt.Fprintf(&text, "retained visits: reference %d, candidate %d, delta %d\nretained child tasks: reference %d, candidate %d, delta %d\n", comparison.StepCount.Reference, comparison.StepCount.Candidate, comparison.StepCount.Delta, comparison.ChildTaskCount.Reference, comparison.ChildTaskCount.Candidate, comparison.ChildTaskCount.Delta)
	for _, row := range comparison.Steps {
		fmt.Fprintf(&text, "\nvisit position %d: node %s\nactual outcome: %s; actual report: %s\nreported outcome: %s; reported report: %s\nelapsed delta milliseconds: %s\n", row.Position, row.Node, row.Outcome, row.Report, row.ReportedOutcome, row.ReportedReport, comparisonNumber(row.DurationDeltaMillis))
		writeComparedStep(&text, "reference", row.Reference)
		writeComparedStep(&text, "candidate", row.Candidate)
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func writeComparedStep(out io.Writer, label string, step *clientapi.SwarmPassComparedStep) {
	if step == nil {
		fmt.Fprintf(out, "  %s visit: missing\n", label)
		return
	}
	fmt.Fprintf(out, "  %s visit: #%d %q (%q)\n    actual outcome: %s\n    actual report: %s\n    reported outcome: %s\n    reported report: %s\n    elapsed milliseconds: %s\n    retained child tasks: %d\n", label, step.Sequence, step.NodeID, step.State, comparisonText(step.Outcome), comparisonText(step.Report), comparisonText(step.ReportedOutcome), comparisonText(step.ReportedReport), comparisonNumber(step.DurationMillis), step.ChildTasks)
}

func comparisonText(value *string) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Quote(*value)
}

func comparisonNumber(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}
