package main

import (
	"errors"
	"strings"
)

type executionOptions struct {
	guest bool
	swarm string
	args  []string
}

func parseExecution(args []string, mode, swarm string) (executionOptions, error) {
	options := executionOptions{guest: mode == "guest", swarm: swarm}
	if mode != "" && mode != "guest" && mode != "operator" {
		return options, errors.New("BROOD_EXECUTION_MODE must be guest or operator")
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			options.args = append(options.args, args[i:]...)
			i = len(args)
		case arg == "--guest":
			options.guest = true
		case arg == "--swarm":
			if i+1 == len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return options, errors.New("--swarm requires an ID")
			}
			i++
			options.swarm = args[i]
		case strings.HasPrefix(arg, "--swarm="):
			options.swarm = strings.TrimPrefix(arg, "--swarm=")
			if options.swarm == "" {
				return options, errors.New("--swarm requires an ID")
			}
		default:
			options.args = append(options.args, arg)
		}
	}
	if options.guest && options.swarm != "" {
		return options, errors.New("guest mode cannot select a swarm")
	}
	return options, nil
}
