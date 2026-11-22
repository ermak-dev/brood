//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/proxmox/relay"
)

type workspacePayload struct {
	WorkspaceOrigin
	sshPrivateKey string
}

// JSON может экранировать байты ключа; отдельно ограничиваем декодированное поле.
const workspacePayloadLimit = workspaceOriginLimit + 6*workspaceSSHKeyLimit

func fetchWorkspace(parent context.Context, cfg relay.ClientConfig) (workspacePayload, error) {
	fail := func() (workspacePayload, error) { return workspacePayload{}, originFailure() }
	route, err := newServerRoute(cfg)
	if err != nil {
		return fail()
	}
	target := *route.origin
	target.Path = route.path("/api/proxmox/workspace/")
	transport := route.transport(5 * time.Second)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return originFailure() }}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fail()
	}
	request.Header.Set("Authorization", "Bearer "+route.token)
	response, err := client.Do(request)
	if err != nil {
		return fail()
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fail()
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, workspacePayloadLimit+1))
	if err != nil || len(data) > workspacePayloadLimit {
		return fail()
	}
	var key string
	origin, err := decodeWorkspaceOrigin(data, &key)
	if err != nil {
		return fail()
	}
	return workspacePayload{WorkspaceOrigin: origin, sshPrivateKey: key}, nil
}

func decodeWorkspaceOrigin(data []byte, sshPrivateKey *string) (WorkspaceOrigin, error) {
	fail := func() (WorkspaceOrigin, error) { return WorkspaceOrigin{}, originFailure() }
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return fail()
	}
	fields := map[string]string{}
	var identity gitidentity.Identity
	var models actormodel.Models
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "origin" && key != "gitIdentity" && key != "models" && (key != "sshPrivateKey" || sshPrivateKey == nil)) {
			return fail()
		}
		if _, exists := fields[key]; exists {
			return fail()
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fail()
		}
		if key == "gitIdentity" {
			// Ровно {name, email} строками: как и origin, без лишних полей и null.
			if identity, ok = decodeGitIdentity(raw); !ok {
				return fail()
			}
			fields[key] = ""
			continue
		}
		if key == "models" {
			// Сервер шлёт только заданные имена (#263): пустой объект и пустое
			// имя — не его ответ, как и null.
			if models, ok = decodeModels(raw); !ok {
				return fail()
			}
			fields[key] = ""
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return fail()
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	_, hasOrigin := fields["origin"]
	key, hasKey := fields["sshPrivateKey"]
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || !hasOrigin || (sshPrivateKey != nil && !hasKey) {
		return fail()
	}
	origin := WorkspaceOrigin{Origin: fields["origin"], GitIdentity: identity, Models: models}
	if checkWorkspaceOrigin(origin) != nil {
		return fail()
	}
	if hasKey {
		if len(key) > workspaceSSHKeyLimit || strings.ContainsRune(key, 0) {
			return fail()
		}
		*sshPrivateKey = key
	}
	return origin, nil
}

// decodeGitIdentity accepts one object of exactly the two identity strings.
func decodeGitIdentity(raw json.RawMessage) (gitidentity.Identity, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 {
		return gitidentity.Identity{}, false
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return gitidentity.Identity{}, false
		}
	}
	var identity gitidentity.Identity
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&identity) != nil || d.Decode(new(any)) != io.EOF || identity.Validate() != nil {
		return gitidentity.Identity{}, false
	}
	return identity, true
}

// decodeModels accepts one nonempty object of the model and smallModel strings,
// each at most once and each a valid model name.
func decodeModels(raw json.RawMessage) (actormodel.Models, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if first, err := decoder.Token(); err != nil || first != json.Delim('{') {
		return actormodel.Models{}, false
	}
	names := map[string]string{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "model" && key != "smallModel") {
			return actormodel.Models{}, false
		}
		if _, exists := names[key]; exists {
			return actormodel.Models{}, false
		}
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || !actormodel.ValidName(name) {
			return actormodel.Models{}, false
		}
		names[key] = name
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(names) == 0 {
		return actormodel.Models{}, false
	}
	return actormodel.Models{Model: names["model"], Small: names["smallModel"]}, true
}

// RefreshWorkspace stages the verified metadata and runtime SSH credential before the actor starts.
func RefreshWorkspace(ctx context.Context, cfg relay.ClientConfig) error {
	if os.Geteuid() != 0 {
		return errors.New("workspace setup requires guest root")
	}
	workspace, err := fetchWorkspace(ctx, cfg)
	if err != nil {
		return err
	}
	if err := prepareRunDirectory(); err != nil {
		return err
	}
	if err := publishWorkspaceSSH(workspace.sshPrivateKey); err != nil {
		return err
	}
	return publishWorkspaceOrigin(projectSecretDirectory, workspace.WorkspaceOrigin, 0, 1000)
}
