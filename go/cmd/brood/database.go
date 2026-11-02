package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"brood.dev/go/brood/store"
)

func cmdDatabase(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "inspect" && args[0] != "snapshot") {
		fmt.Fprintln(stderr, "brood database: expected inspect or snapshot")
		return 1
	}
	action := args[0]
	flags := flag.NewFlagSet("database", flag.ContinueOnError)
	// flag по умолчанию печатает ошибочные значения аргументов: не отдаём
	// их в логи deployment job, где путь/ввод могут содержать секрет.
	flags.SetOutput(io.Discard)
	path := flags.String("path", "", "existing SQLite source database")
	timeout := flags.Duration("timeout", 10*time.Second, "operation timeout (at most 1m)")
	var output string
	if action == "snapshot" {
		flags.StringVar(&output, "output", "", "new private snapshot file")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "brood database: invalid arguments; use --path, --timeout, and --output for snapshot")
		return 1
	}
	if *path == "" || (action == "snapshot" && output == "") {
		fmt.Fprintln(stderr, "brood database: --path is required; snapshot also requires --output")
		return 1
	}
	if *timeout <= 0 || *timeout > time.Minute {
		fmt.Fprintln(stderr, "brood database: --timeout must be positive and at most 1m")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var metadata any
	var err error
	if action == "inspect" {
		metadata, err = store.InspectDatabase(ctx, *path)
	} else {
		metadata, err = store.SnapshotDatabase(ctx, *path, output)
	}
	if err != nil {
		fmt.Fprintf(stderr, "brood database: %s\n", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(metadata); err != nil {
		fmt.Fprintln(stderr, "brood database: cannot write metadata JSON")
		return 1
	}
	return 0
}
