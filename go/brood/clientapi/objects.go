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

// Agent is one agent with its placement, as the server shows it. Phase is the
// state of its container; Error explains the phase "error".
type Agent struct {
	Name          string `json:"name"`
	Swarm         string `json:"swarm"`
	SwarmName     string `json:"swarmName"`
	ProjectID     string `json:"projectId"`
	ProjectName   string `json:"projectName"`
	Role          string `json:"role"`
	Phase         string `json:"phase"`
	Desired       string `json:"desired"`
	Pool          string `json:"pool"`
	Node          string `json:"node"`
	CTID          int64  `json:"ctid"`
	Profile       string `json:"profile"`
	Origin        string `json:"origin"`
	CPUMillicores int64  `json:"cpuMillicores"`
	MemoryBytes   int64  `json:"memoryBytes"`
	DiskBytes     int64  `json:"diskBytes"`
	Error         string `json:"error"`
	ErrorCode     string `json:"errorCode"`
	CreatedAt     string `json:"createdAt"`
	// Work of a running agent; nil for any other agent.
	Work *AgentWork `json:"work"`
}

// AgentWork is what a running agent does and what its clone holds. Activity
// is the verdict: free, busy, idle, needs_action, waiting, error, offline or
// unknown; Detail explains it. Uncommitted and Unpushed are nil when the
// clone was not measured.
type AgentWork struct {
	Activity    string `json:"activity"`
	Detail      string `json:"detail"`
	Branch      string `json:"branch"`
	Uncommitted *int   `json:"uncommitted"`
	Unpushed    *int   `json:"unpushed"`
	Model       string `json:"model"`
}

// AgentRequest describes a new agent. Pool is a name from PoolProfiles. An
// empty Role and Origin leave the choice to the server.
type AgentRequest struct {
	Name   string `json:"name"`
	Pool   string `json:"pool"`
	Role   string `json:"roleId,omitempty"`
	Origin string `json:"origin,omitempty"`
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
// is empty.
func (c *Client) Agents(ctx context.Context, swarm string) ([]Agent, error) {
	path := "/api/agents"
	if swarm != "" {
		path = agentsPath(swarm)
	}
	var agents []Agent
	if err := c.call(ctx, http.MethodGet, path, nil, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// Agent returns one agent of the swarm with its current phase.
func (c *Client) Agent(ctx context.Context, swarm, name string) (Agent, error) {
	var agent Agent
	err := c.call(ctx, http.MethodGet, agentsPath(swarm)+"/"+url.PathEscape(name), nil, &agent)
	return agent, err
}

// AgentCreate asks the server to create an agent and returns it in its
// initial phase; the container is provisioned afterwards.
func (c *Client) AgentCreate(ctx context.Context, swarm string, request AgentRequest) (Agent, error) {
	if request.Name == "" || request.Pool == "" {
		return Agent{}, errors.New("agent name and pool are required")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Agent{}, err
	}
	var agent Agent
	err = c.call(ctx, http.MethodPost, agentsPath(swarm), body, &agent)
	return agent, err
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

// PoolProfiles lists the names of the pools an agent of the swarm can be
// created in.
func (c *Client) PoolProfiles(ctx context.Context, swarm string) ([]string, error) {
	var names []string
	if err := c.call(ctx, http.MethodGet, "/api/swarms/"+url.PathEscape(swarm)+"/profiles", nil, &names); err != nil {
		return nil, err
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
	return "/api/swarms/" + url.PathEscape(swarm) + "/agents"
}
