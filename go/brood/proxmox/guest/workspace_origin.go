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
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/proxmox/relay"
)

// FetchWorkspaceOrigin reads only this actor generation's server-assigned origin.
func FetchWorkspaceOrigin(parent context.Context, cfg relay.ClientConfig) (WorkspaceOrigin, error) {
	fail := func() (WorkspaceOrigin, error) { return WorkspaceOrigin{}, originFailure() }
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
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<10)+1))
	if err != nil || len(data) > 4<<10 {
		return fail()
	}
	origin, err := decodeWorkspaceOrigin(data)
	if err != nil || ValidateWorkspaceOrigin(origin.Origin, origin.Mode) != nil {
		return fail()
	}
	return origin, nil
}

func decodeWorkspaceOrigin(data []byte) (WorkspaceOrigin, error) {
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
		if err != nil || !ok || (key != "origin" && key != "mode" && key != "gitIdentity" && key != "models") {
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
	_, hasMode := fields["mode"]
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || !hasOrigin || !hasMode {
		return fail()
	}
	origin := WorkspaceOrigin{Origin: fields["origin"], Mode: fields["mode"], GitIdentity: identity, Models: models}
	if checkWorkspaceOrigin(origin) != nil {
		return fail()
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

// RefreshWorkspaceOrigin stages the verified response for the UID 1000 wrapper.
func RefreshWorkspaceOrigin(ctx context.Context, cfg relay.ClientConfig) (WorkspaceOrigin, error) {
	if os.Geteuid() != 0 {
		return WorkspaceOrigin{}, errors.New("workspace origin requires guest root")
	}
	origin, err := FetchWorkspaceOrigin(ctx, cfg)
	if err != nil {
		return WorkspaceOrigin{}, err
	}
	if err := PublishWorkspaceOrigin(origin); err != nil {
		return WorkspaceOrigin{}, err
	}
	return origin, nil
}
