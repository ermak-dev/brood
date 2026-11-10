//go:build linux

package main

import (
	"context"
	"encoding/json"
	"os"

	"brood.dev/go/brood/proxmox/guest"
)

func runMessageHelper(ctx context.Context) error {
	request, err := guest.ReadDeliveryRequest(os.Stdin)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(guest.ReceiveMessage(ctx, request))
}
