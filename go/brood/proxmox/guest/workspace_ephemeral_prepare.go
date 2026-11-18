//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

const ephemeralPreparationOperation = "workspace_prepare_ephemeral"

func decodeEphemeralPreparation(raw json.RawMessage) (sessionctl.CheckoutRequest, error) {
	var request sessionctl.CheckoutRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF || !validPinnedCommit(request.Commit) {
		return sessionctl.CheckoutRequest{}, errors.New("invalid ephemeral workspace preparation arguments")
	}
	if err := checkWorkspaceOrigin(WorkspaceOrigin{Origin: request.Origin, Mode: request.Mode, GitIdentity: request.GitIdentity, Ephemeral: &EphemeralWorkspace{Commit: request.Commit}}); err != nil {
		return sessionctl.CheckoutRequest{}, errors.New("invalid ephemeral workspace preparation source")
	}
	return request, nil
}

func prepareEphemeralWorkspace(ctx context.Context, layout appLayout, request sessionctl.CheckoutRequest) error {
	origin, err := readWorkspaceOrigin(layout.Run, layout.RunUID, layout.RunGID)
	if err != nil {
		return err
	}
	if origin.Ephemeral == nil || origin.Ephemeral.Commit != request.Commit || origin.Origin != request.Origin || origin.Mode != request.Mode || origin.GitIdentity != request.GitIdentity {
		return errors.New("ephemeral workspace preparation differs from the root bootstrap projection")
	}
	return cloneWorkspace(ctx, cloneSpec{Project: layout.Project, Home: layout.Home, Origin: origin.Origin, Mode: origin.Mode, Commit: origin.Ephemeral.Commit, Identity: origin.GitIdentity, Gateway: layout.Gateway, Timeout: layout.CloneTimeout})
}

func receiveEphemeralPreparation(ctx context.Context, request relay.SessionRequest) relay.SessionResponse {
	args, err := decodeEphemeralPreparation(request.Arguments)
	if err != nil {
		return attestFailure(err.Error())
	}
	layout := appLayout{Project: "/workspace/project", Home: "/workspace/home", Run: projectSecretDirectory, RunUID: 0, RunGID: 1000, Gateway: workspaceGatewayURL, CloneTimeout: checkoutHelperTimeout}
	if err := prepareEphemeralWorkspace(ctx, layout, args); err != nil {
		return attestFailure(err.Error())
	}
	return relay.SessionResponse{OK: true, Outcome: "complete"}
}

func runEphemeralPreparation(ctx context.Context, request sessionctl.CheckoutRequest) error {
	data, err := json.Marshal(request)
	if err != nil {
		return errors.New("ephemeral workspace preparation request encoding failed")
	}
	response := runHelper(ctx, relay.SessionRequest{Operation: ephemeralPreparationOperation, Arguments: data, Caller: "supervisor"}, checkoutHelperTimeout)
	if !response.OK || response.Outcome != "complete" {
		if response.Text != "" {
			return errors.New(response.Text)
		}
		return errors.New("ephemeral workspace preparation failed")
	}
	return nil
}
