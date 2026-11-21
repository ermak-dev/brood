package main

import (
	"errors"
	"strings"
)

// scope — рой, названный запуском: опцией и окружением. Опция важнее.
type scope struct {
	flag, env string
}

type executionOptions struct {
	guest bool
	scope scope
	args  []string
}

func parseExecution(args []string, mode, swarm string) (executionOptions, error) {
	options := executionOptions{guest: mode == "guest", scope: scope{env: swarm}}
	if mode != "" && mode != "guest" && mode != "operator" {
		return options, errors.New("BROOD_EXECUTION_MODE must be guest or operator")
	}
	words := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--" || verbatimFrom(options.args, words):
			options.args = append(options.args, args[i:]...)
			i = len(args)
		case arg == "--guest":
			options.guest = true
		case arg == "--swarm" || arg == "-s":
			if i+1 == len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return options, errors.New("--swarm requires a swarm name")
			}
			i++
			options.scope.flag = args[i]
		case strings.HasPrefix(arg, "--swarm="):
			options.scope.flag = strings.TrimPrefix(arg, "--swarm=")
			if options.scope.flag == "" {
				return options, errors.New("--swarm requires a swarm name")
			}
		default:
			if !strings.HasPrefix(arg, "-") {
				words++
			}
			options.args = append(options.args, arg)
		}
	}
	if options.guest && (options.scope.flag != "" || options.scope.env != "") {
		return options, errors.New("guest mode cannot select a swarm")
	}
	return options, nil
}

// verbatimFrom сообщает, что дальше идёт текст сообщения send: в нём -s и
// --guest — слова человека, а не опции запуска.
func verbatimFrom(args []string, words int) bool {
	return len(args) > 0 && args[0] == "send" && words >= 3
}
