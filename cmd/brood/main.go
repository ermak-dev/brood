// brood — единая точка входа: диспетчер командлетов. Ничто не запускается
// в обход него.
package main

import (
	"fmt"
	"os"

	"git.ermak.dev/ermak/brood/internal/cli"
	"git.ermak.dev/ermak/brood/internal/cli/dev/bug"
	"git.ermak.dev/ermak/brood/internal/cli/dev/ci"
	"git.ermak.dev/ermak/brood/internal/env"
)

const usage = `brood — an agent pool orchestrator on Agent Substrate

  brood dev bug   project tracker: GitLab issues, one issue per unit of work
  brood dev ci    pipelines, job logs, lint and runners: GitLab CI from the terminal

Coordinates come from the working copy's git origin; credentials live in
.env (GITLAB_TOKEN). Run from a working copy of the project.
`

func main() {
	env.Load(env.EnvPath())
	argv := os.Args[1:]
	if len(argv) == 0 {
		cli.Usage(usage)
	}
	if argv[0] == "dev" {
		if len(argv) < 2 {
			cli.Usage(usage)
		}
		switch argv[1] {
		case "bug":
			bug.Main(argv[2:])
		case "ci":
			ci.Main(argv[2:])
		default:
			cli.Usage(usage)
		}
		return
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}
