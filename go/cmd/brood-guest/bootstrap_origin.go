//go:build linux

package main

import (
	"context"
	"errors"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

func refreshWorkspaceBootstrap(ctx context.Context, config relay.ClientConfig, hosts []string) (guest.WorkspaceOrigin, error) {
	return fetchAndPublishWorkspaceBootstrap(ctx, config, hosts, guest.PublishWorkspaceOrigin)
}

func fetchAndPublishWorkspaceBootstrap(ctx context.Context, config relay.ClientConfig, hosts []string, publish func(guest.WorkspaceOrigin) error) (guest.WorkspaceOrigin, error) {
	if publish == nil {
		return guest.WorkspaceOrigin{}, errors.New("workspace bootstrap publisher is unavailable")
	}
	origin, err := guest.FetchWorkspaceBootstrap(ctx, config, hosts)
	if err != nil {
		return guest.WorkspaceOrigin{}, err
	}
	if err := publish(origin); err != nil {
		return guest.WorkspaceOrigin{}, err
	}
	return origin, nil
}
