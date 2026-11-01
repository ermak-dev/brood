//go:build linux

package main

import (
	"context"
	"errors"
	"os"

	"brood.dev/go/brood/proxmox/guest"
)

func runMCP(ctx context.Context) error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return errors.New("MCP requires actor UID and GID 1000")
	}
	stop := context.AfterFunc(ctx, func() { os.Stdin.Close() })
	defer stop()
	return guest.ServeMCP(ctx, os.Stdin, os.Stdout, guest.CallSwarm)
}
