package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SwarmRoleGraph names the graph of a role view: the pinned snapshot of the
// swarm or a catalog definition before the first start.
type SwarmRoleGraph struct {
	Pinned   bool   `json:"pinned"`
	Name     string `json:"name"`
	Version  int64  `json:"version"`
	Revision string `json:"revision"`
}

// SwarmRole is one role of the graph or of a stored binding. State is the
// verdict a swarm start would give it by its actor: ready, unbound, stale,
// inactive, foreign or missing.
type SwarmRole struct {
	Role             string `json:"role"`
	InGraph          bool   `json:"inGraph"`
	State            string `json:"state"`
	ActorUID         string `json:"actorUid,omitempty"`
	ActorName        string `json:"actorName,omitempty"`
	ActorIncarnation int64  `json:"actorIncarnation,omitempty"`
}

// SwarmRoles is the role view of a swarm (go/CONTRACT.md).
type SwarmRoles struct {
	SwarmID   string          `json:"swarmId"`
	Execution string          `json:"execution"`
	Graph     *SwarmRoleGraph `json:"graph"`
	Roles     []SwarmRole     `json:"roles"`
}

// SwarmActor is a managed actor of a swarm, the kind a role binds to.
type SwarmActor struct {
	UID         string
	Name        string
	Incarnation int64
	Phase       string
}

// SwarmRoles reads the role view of a swarm; a non-empty definition names
// the catalog workflow whose roles to list before the first start.
func (c *Client) SwarmRoles(ctx context.Context, swarm, definition string) (SwarmRoles, error) {
	if swarm == "" {
		return SwarmRoles{}, errors.New("swarm is required")
	}
	path := swarmRolesPath(swarm, "")
	if definition != "" {
		path += "?" + url.Values{"definition": {definition}}.Encode()
	}
	return c.swarmRolesCall(ctx, http.MethodGet, path, nil)
}

// BindSwarmRole binds a role to a managed actor of the swarm; a positive
// incarnation is the one the caller resolved, and the server refuses the
// binding when the actor has changed since.
func (c *Client) BindSwarmRole(ctx context.Context, swarm, role, actorUID string, incarnation int64) (SwarmRoles, error) {
	if swarm == "" || role == "" || actorUID == "" || incarnation < 0 {
		return SwarmRoles{}, errors.New("swarm, role and actor are required")
	}
	body, err := json.Marshal(struct {
		ActorUID         string `json:"actorUid"`
		ActorIncarnation int64  `json:"actorIncarnation,omitempty"`
	}{actorUID, incarnation})
	if err != nil {
		return SwarmRoles{}, errors.New("could not encode role binding")
	}
	return c.swarmRolesCall(ctx, http.MethodPut, swarmRolesPath(swarm, role), body)
}

// UnbindSwarmRole removes the binding of a role.
func (c *Client) UnbindSwarmRole(ctx context.Context, swarm, role string) (SwarmRoles, error) {
	if swarm == "" || role == "" {
		return SwarmRoles{}, errors.New("swarm and role are required")
	}
	return c.swarmRolesCall(ctx, http.MethodDelete, swarmRolesPath(swarm, role), nil)
}

// SwarmActors lists the managed actors of a swarm; native actors cannot
// hold a role and are left out.
func (c *Client) SwarmActors(ctx context.Context, swarm string) ([]SwarmActor, error) {
	if swarm == "" {
		return nil, errors.New("swarm is required")
	}
	var list struct {
		Results []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
			Provider   string `json:"provider"`
			Generation string `json:"generation"`
			Phase      string `json:"phase"`
		} `json:"results"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/swarms/"+url.PathEscape(swarm)+"/actors", nil, &list); err != nil {
		return nil, err
	}
	actors := []SwarmActor{}
	for _, row := range list.Results {
		if row.Provider != "proxmox" {
			continue
		}
		incarnation, err := strconv.ParseInt(row.Generation, 10, 64)
		if err != nil || incarnation <= 0 || row.Metadata.UID == "" {
			return nil, errors.New("malformed managed actor in the swarm actor list")
		}
		actors = append(actors, SwarmActor{UID: row.Metadata.UID, Name: row.Metadata.Name, Incarnation: incarnation, Phase: row.Phase})
	}
	return actors, nil
}

func (c *Client) swarmRolesCall(ctx context.Context, method, path string, body []byte) (SwarmRoles, error) {
	var view SwarmRoles
	if err := c.call(ctx, method, path, body, &view); err != nil {
		return SwarmRoles{}, err
	}
	if view.Roles == nil {
		view.Roles = []SwarmRole{}
	}
	return view, nil
}

func swarmRolesPath(swarm, role string) string {
	path := "/api/swarms/" + url.PathEscape(swarm) + "/roles"
	if role != "" {
		path += "/" + url.PathEscape(role)
	}
	return path
}

// isSwarmRolesPath узнаёт маршруты привязки ролей: /api/swarms/{id}/roles
// и /api/swarms/{id}/roles/{role}; сегменты экранированы, «/» внутри нет.
func isSwarmRolesPath(path string) bool {
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(path, "/")
	return (len(parts) == 5 || len(parts) == 6) && parts[0] == "" && parts[1] == "api" && parts[2] == "swarms" && parts[4] == "roles"
}

// swarmRoleRefusals — фиксированные причины отказов API ролей роя
// (go/CONTRACT.md). Тело ответа недоверенное: в ошибку попадает только
// точное совпадение с этим набором.
var swarmRoleRefusals = map[string]bool{
	"invalid swarm role binding: expected {actorUid, actorIncarnation?}": true,
	"swarm role must be a lowercase DNS label":                           true,
	"swarm management denied":                                            true,
	"swarm not found":                                                    true,
	"swarm role is not bound":                                            true,
	"workflow definition is not in the project catalog":                  true,
	"swarm is not active":                                                true,
	"swarm execution is running or waiting; stop the swarm before changing role bindings": true,
	"actor is not an actor of this swarm":                                                 true,
	"actor is not live: it must be provisioning or running and meant to run":              true,
	"actor incarnation changed; list the swarm actors and bind again":                     true,
	"swarm graph is pinned to another workflow definition":                                true,
	"workflow graph is not executable":                                                    true,
}

// publicSwarmRoleError — причина отказа API ролей, если она из набора.
func publicSwarmRoleError(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4097))
	if err != nil || len(data) > 4096 {
		return ""
	}
	var response struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(data, &response) != nil || !swarmRoleRefusals[response.Detail] {
		return ""
	}
	return response.Detail
}
