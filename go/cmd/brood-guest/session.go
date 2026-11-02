//go:build linux

package main

import (
	"context"
	"encoding/json"
	"os"

	"brood.dev/go/brood/proxmox/guest"
)

func runSessionHelper(ctx context.Context) error {
	request, err := guest.ReadSessionRequest(os.Stdin)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(guest.ReceiveSession(ctx, request))
}
