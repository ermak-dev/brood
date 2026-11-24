package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// Swarm is a group of agents with one owner, as GET /api/swarms lists it.
type Swarm struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	Owner       string `json:"owner_username"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	AgentCount  int64  `json:"agentCount"`
}

// Agent is one agent with its placement. Phase is the observed state of its
// container; ErrorMessage explains the phase "error".
type Agent struct {
	Name         string `json:"name"`
	Swarm        string `json:"swarm"`
	SwarmName    string `json:"swarm_name,omitempty"`
	ProjectName  string `json:"project_name,omitempty"`
	Role         string `json:"role"`
	Phase        string `json:"phase"`
	Pool         string `json:"pool"`
	CTID         int64  `json:"ctid"`
	Origin       string `json:"origin"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    string `json:"created_at"`
	// Work of a running agent; Activity is empty for any other agent.
	// Uncommitted and Unpushed are nil when the clone was not measured.
	Activity    string `json:"activity,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Uncommitted *int   `json:"uncommitted,omitempty"`
	Unpushed    *int   `json:"unpushed,omitempty"`
	Model       string `json:"model,omitempty"`
}

// agentWire — форма агента в ответах сервера.
type agentWire struct {
	Metadata struct {
		Name  string `json:"name"`
		Swarm string `json:"swarm"`
	} `json:"metadata"`
	SwarmName   string `json:"swarmName"`
	ProjectName string `json:"projectName"`
	Role        string `json:"role"`
	Phase       string `json:"phase"`
	Origin      string `json:"origin"`
	CreatedAt   string `json:"createdAt"`
	Placement   struct {
		Pool string `json:"pool"`
		CTID int64  `json:"ctid"`
	} `json:"placement"`
	Status struct {
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"status"`
	Work struct {
		Activity    string `json:"activity"`
		Detail      string `json:"detail"`
		Branch      string `json:"branch"`
		Uncommitted *int   `json:"uncommitted"`
		Unpushed    *int   `json:"unpushed"`
		Model       string `json:"model"`
	} `json:"work"`
}

func (w agentWire) agent() Agent {
	return Agent{
		Name: w.Metadata.Name, Swarm: w.Metadata.Swarm, SwarmName: w.SwarmName, ProjectName: w.ProjectName,
		Role: w.Role, Phase: w.Phase, Pool: w.Placement.Pool, CTID: w.Placement.CTID, Origin: w.Origin,
		ErrorCode: w.Status.ErrorCode, ErrorMessage: w.Status.ErrorMessage, CreatedAt: w.CreatedAt,
		Activity: w.Work.Activity, Detail: w.Work.Detail, Branch: w.Work.Branch,
		Uncommitted: w.Work.Uncommitted, Unpushed: w.Work.Unpushed, Model: w.Work.Model,
	}
}

// AgentRequest describes a new agent. Template is a pool profile from
// PoolProfiles. An empty Role and Origin leave the choice to the server.
type AgentRequest struct {
	Name, Template, Role, Origin string
}

// Pool is a Proxmox pool with its limits and current use. State is empty for
// a connection that was never activated as a pool.
type Pool struct {
	Name              string `json:"name"`
	State             string `json:"state"`
	Available         bool   `json:"available"`
	MaxAgents         int64  `json:"max_agents"`
	UsedAgents        int64  `json:"used_agents"`
	CPUMillicores     int64  `json:"cpu_millicores"`
	UsedCPUMillicores int64  `json:"used_cpu_millicores"`
	MemoryBytes       int64  `json:"memory_bytes"`
	UsedMemoryBytes   int64  `json:"used_memory_bytes"`
}

// Swarms lists the swarms the user may use.
func (c *Client) Swarms(ctx context.Context) ([]Swarm, error) {
	var swarms []Swarm
	if err := c.call(ctx, http.MethodGet, "/api/swarms", nil, &swarms); err != nil {
		return nil, err
	}
	return swarms, nil
}

// SwarmCreate creates a swarm in the project; the caller becomes its owner.
func (c *Client) SwarmCreate(ctx context.Context, project, name string) (Swarm, error) {
	var swarm Swarm
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return swarm, err
	}
	err = c.call(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(project)+"/swarms", body, &swarm)
	return swarm, err
}

// SwarmDelete deletes a swarm; the server refuses while it has agents.
func (c *Client) SwarmDelete(ctx context.Context, swarm string) error {
	return c.call(ctx, http.MethodDelete, "/api/swarms/"+url.PathEscape(swarm), nil, nil)
}

// Agents lists the agents of one swarm, or of every usable swarm when swarm
// is empty; only the latter fills SwarmName and ProjectName.
func (c *Client) Agents(ctx context.Context, swarm string) ([]Agent, error) {
	path := "/api/agents"
	if swarm != "" {
		path = agentsPath(swarm)
	}
	var list struct {
		Results []agentWire `json:"results"`
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	agents := make([]Agent, 0, len(list.Results))
	for _, wire := range list.Results {
		agents = append(agents, wire.agent())
	}
	return agents, nil
}

// Agent returns one agent of the swarm with its current phase.
func (c *Client) Agent(ctx context.Context, swarm, name string) (Agent, error) {
	var wire agentWire
	if err := c.call(ctx, http.MethodGet, agentsPath(swarm)+"/"+url.PathEscape(name), nil, &wire); err != nil {
		return Agent{}, err
	}
	return wire.agent(), nil
}

// AgentCreate asks the server to create an agent and returns it in its
// initial phase; the container is provisioned afterwards.
func (c *Client) AgentCreate(ctx context.Context, swarm string, request AgentRequest) (Agent, error) {
	if request.Name == "" || request.Template == "" {
		return Agent{}, errors.New("agent name and pool profile are required")
	}
	fields := map[string]any{
		"metadata":      map[string]string{"name": request.Name},
		"actorTemplate": map[string]string{"name": request.Template},
	}
	if request.Role != "" {
		fields["roleId"] = request.Role
	}
	if request.Origin != "" {
		fields["origin"] = request.Origin
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return Agent{}, err
	}
	var wire agentWire
	if err := c.call(ctx, http.MethodPost, agentsPath(swarm), body, &wire); err != nil {
		return Agent{}, err
	}
	return wire.agent(), nil
}

type AgentRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) AgentRoles(ctx context.Context) ([]AgentRole, error) {
	var roles []AgentRole
	err := c.call(ctx, http.MethodGet, "/api/roles", nil, &roles)
	return roles, err
}

// PoolProfiles lists the names of the pool profiles an agent of the swarm
// can be created from.
func (c *Client) PoolProfiles(ctx context.Context, swarm string) ([]string, error) {
	var list struct {
		Results []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"results"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/swarms/"+url.PathEscape(swarm)+"/templates", nil, &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Results))
	for _, profile := range list.Results {
		names = append(names, profile.Metadata.Name)
	}
	return names, nil
}

// Pools lists the Proxmox pools; the server allows it to administrators.
func (c *Client) Pools(ctx context.Context) ([]Pool, error) {
	var list struct {
		Items []struct {
			Name      string `json:"name"`
			Available bool   `json:"runtimeAvailable"`
			Managed   *struct {
				State             string `json:"state"`
				MaxAgents         int64  `json:"maxActors,string"`
				UsedAgents        int64  `json:"usedActors,string"`
				CPUMillicores     int64  `json:"cpuMillicores,string"`
				UsedCPUMillicores int64  `json:"usedCPUMillicores,string"`
				MemoryBytes       int64  `json:"memoryBytes,string"`
				UsedMemoryBytes   int64  `json:"usedMemoryBytes,string"`
			} `json:"managedPool"`
		} `json:"items"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/proxmox-pools", nil, &list); err != nil {
		return nil, err
	}
	pools := make([]Pool, 0, len(list.Items))
	for _, item := range list.Items {
		pool := Pool{Name: item.Name, Available: item.Available}
		if m := item.Managed; m != nil {
			pool.State, pool.MaxAgents, pool.UsedAgents = m.State, m.MaxAgents, m.UsedAgents
			pool.CPUMillicores, pool.UsedCPUMillicores = m.CPUMillicores, m.UsedCPUMillicores
			pool.MemoryBytes, pool.UsedMemoryBytes = m.MemoryBytes, m.UsedMemoryBytes
		}
		pools = append(pools, pool)
	}
	return pools, nil
}

func agentsPath(swarm string) string {
	return "/api/swarms/" + url.PathEscape(swarm) + "/actors"
}
