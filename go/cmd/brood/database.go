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
	if len(args) == 0 || (args[0] != "inspect" && args[0] != "snapshot" && args[0] != "verify") {
		fmt.Fprintln(stderr, "brood database: expected inspect, snapshot or verify")
		return 1
	}
	action := args[0]
	flags := flag.NewFlagSet("database", flag.ContinueOnError)
	// flag по умолчанию печатает ошибочные значения аргументов: не отдаём
	// их в логи deployment job, где путь/ввод могут содержать секрет.
	flags.SetOutput(io.Discard)
	path := flags.String("path", "", "existing SQLite source database")
	timeout := flags.Duration("timeout", 10*time.Second, "operation timeout (at most 1m)")
	var output, wantSHA string
	if action == "snapshot" {
		flags.StringVar(&output, "output", "", "new private snapshot file")
	}
	if action == "verify" {
		flags.StringVar(&wantSHA, "sha256", "", "expected SHA-256 of the snapshot")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "brood database: invalid arguments; use --path, --timeout, and --output for snapshot")
		return 1
	}
	if action == "verify" && wantSHA == "" {
		fmt.Fprintln(stderr, "brood database: verify requires --path and --sha256")
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
	switch action {
	case "inspect":
		metadata, err = store.InspectDatabase(ctx, *path)
	case "verify":
		metadata, err = store.VerifyDatabaseSchema(ctx, *path, wantSHA)
	default:
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
