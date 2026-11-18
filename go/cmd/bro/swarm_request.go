package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"brood.dev/go/brood/clientapi"
)

type swarmRequestOptions struct {
	action, swarm, request string
	all                    bool
	answer                 clientapi.SwarmRequestAnswer
}

func parseSwarmRequest(args []string) (swarmRequestOptions, error) {
	var result swarmRequestOptions
	var positional []string
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		if arg == "--all" && !result.all {
			result.all = true
			continue
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !strings.HasPrefix(arg, "--") || name != "response" && name != "outcome" && name != "revision" {
			return result, errors.New("invalid request option; use bro swarm --help")
		}
		if _, repeated := values[name]; repeated {
			return result, fmt.Errorf("--%s must be specified once", name)
		}
		if !inline {
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
				return result, fmt.Errorf("--%s requires a value", name)
			}
			i++
			value = args[i]
		}
		values[name] = value
	}
	if len(positional) == 0 {
		return result, errors.New("request action is required")
	}
	result.action = positional[0]
	switch result.action {
	case "requests":
		if len(positional) != 2 || len(values) != 0 {
			return result, errors.New("usage: bro swarm requests SWARM [--all]")
		}
	case "answer":
		if len(positional) != 3 || result.all || len(values) == 0 {
			return result, errors.New("usage: bro swarm answer SWARM REQUEST [--outcome VALUE] [--response TEXT] [--revision N]")
		}
		result.request = positional[2]
		result.answer.Outcome, result.answer.Response = values["outcome"], values["response"]
		if value, exists := values["revision"]; exists {
			revision, err := strconv.ParseInt(value, 10, 64)
			if err != nil || revision < 1 {
				return result, errors.New("--revision requires a positive integer")
			}
			result.answer.Revision = revision
		}
		if strings.HasPrefix(result.request, "policy:") {
			if result.answer.Revision != 1 || result.answer.Outcome != "" || result.answer.Response != "yes" && result.answer.Response != "no" {
				return result, errors.New("policy confirmations require --response yes or no and --revision 1")
			}
		} else if result.answer.Revision != 0 {
			return result, errors.New("--revision is only valid for policy confirmations")
		}
	default:
		return result, errors.New("request action must be requests or answer")
	}
	result.swarm = positional[1]
	if result.swarm == "" || result.action == "answer" && result.request == "" || len(result.answer.Response) > 16384 {
		return result, errors.New("bounded swarm, request and response are required")
	}
	return result, nil
}

func (c command) swarmRequest(ctx context.Context, args []string) error {
	o, err := parseSwarmRequest(args)
	if err != nil {
		return err
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	swarm, err := resolveSwarm(ctx, client, o.swarm)
	if err != nil {
		return sessionError(err)
	}
	if o.action == "requests" {
		requests, err := client.SwarmRequests(ctx, swarm.ID, o.all)
		if err != nil {
			return sessionError(err)
		}
		if err := sessions.Save(client.Origin(), client.Stored()); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "ID\tORIGIN\tKIND\tSTATE\tREVISION\tQUESTION")
		for _, request := range requests {
			fmt.Fprintf(c.out, "%s\t%s\t%s\t%s\t%d\t%q\n", request.ID, request.Origin, request.Kind, request.State, request.Revision, request.Question)
			if request.Origin == "policy" && request.Context != "" {
				fmt.Fprintf(c.out, "policy context: %q\n", request.Context)
			}
		}
		return nil
	}
	answer, err := client.AnswerSwarmRequest(ctx, swarm.ID, o.request, o.answer)
	if err != nil {
		return sessionError(err)
	}
	if err := sessions.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "recorded %s: %s (revision %d)\n", answer.ID, answer.State, answer.Revision)
	return nil
}
