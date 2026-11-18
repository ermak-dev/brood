package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/app"
	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/proxmox/egress"
	"brood.dev/go/brood/store"
)

// swarmEvalRuntime reads retained isolation policy, never current authorization.
// Only ErrNotFound means ordinary execution; failed reads must stop effects.
func (s *Server) swarmEvalRuntime(ctx context.Context, swarmID string) (*store.SwarmEvalBinding, error) {
	b, err := s.store.GetSwarmEvalBinding(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eval isolation policy is unavailable")
	}
	if !actormodel.ValidName(b.Model) {
		return nil, fmt.Errorf("eval isolation model is unavailable")
	}
	return &b, nil
}

func (s *Server) refuseEvalEffect(ctx context.Context, swarmID, action string) error {
	b, err := s.swarmEvalRuntime(ctx, swarmID)
	if err != nil {
		return err
	}
	if b != nil {
		return fmt.Errorf("%w: eval isolation forbids %s", app.ErrConflict, action)
	}
	return nil
}

// runtimeGitAccess never falls back to installation credentials for eval.
func (s *Server) runtimeGitAccess(ctx context.Context, projectID string, b *store.SwarmEvalBinding) (gitAccess, error) {
	if b == nil {
		return s.projectGitAccess(ctx, projectID)
	}
	if b.CandidateProjectID != projectID {
		return gitAccess{}, fmt.Errorf("eval candidate project binding changed")
	}
	stored, err := s.store.ProjectGitAccess(ctx, projectID)
	if err != nil {
		return gitAccess{}, err
	}
	if !stored.HasCredential() {
		return gitAccess{}, &originRefusal{detail: "eval requires a dedicated candidate project Git credential"}
	}
	return gitAccess{repository: stored.Repository, credential: gitCredential{username: stored.Credential.Username, token: stored.Credential.Token}, gateway: true, bound: true}, nil
}

func (s *Server) evalWorkspaceOrigin(ctx context.Context, projectID, origin, network string, b *store.SwarmEvalBinding) error {
	if b == nil {
		return nil
	}
	if network == "" {
		return &originRefusal{detail: "eval requires an explicit network policy"}
	}
	if _, err := egress.Decode([]byte(network)); err != nil {
		return &originRefusal{detail: "eval requires a valid explicit network policy"}
	}
	access, err := s.runtimeGitAccess(ctx, projectID, b)
	if err != nil {
		return err
	}
	if origin == "" || access.modeWithNetwork(origin, network) != gitorigin.ModeGateway {
		return &originRefusal{detail: "eval requires a gateway origin with candidate project Git access"}
	}
	return nil
}

// evalRequestModel rejects ambiguous top-level model fields before proxy I/O.
// Raw bytes remain unchanged; accepting case-insensitive or duplicate names
// could make this guard and the upstream interpret different models.
func evalRequestModel(body []byte, model string) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	found := false
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok {
			return false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return false
		}
		if strings.EqualFold(key, "model") {
			if key != "model" || found {
				return false
			}
			var got string
			if json.Unmarshal(value, &got) != nil || got != model {
				return false
			}
			found = true
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return false
	}
	return found && d.Decode(new(any)) == io.EOF
}
