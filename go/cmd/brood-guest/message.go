//go:build linux

package main

import (
	"context"
	"encoding/json"
	"os"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

func runMessageHelper(ctx context.Context) error {
	request, err := guest.ReadDeliveryRequest(os.Stdin)
	if err != nil {
		return err
	}
	response, err := guest.ReceiveMessage(ctx, request)
	if err != nil {
		response = relay.MessageResponse{Verdict: err.Error()}
	}
	return json.NewEncoder(os.Stdout).Encode(response)
}
