package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const agentRoleSchema = `CREATE TABLE agent_roles (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    prompt TEXT NOT NULL,
    permissions TEXT NOT NULL,
    is_default INTEGER NOT NULL CHECK (is_default IN (0, 1)),
    UNIQUE (project_id, name)
)`

const agentRoleDefaultIndex = `CREATE UNIQUE INDEX agent_roles_default ON agent_roles(project_id) WHERE is_default = 1`

func verifyAgentRoleSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, agentRoleSchema); err != nil {
		return err
	}
	return checkObject(ctx, tx, "index", "agent_roles_default", agentRoleDefaultIndex)
}

// AgentRole is a project-scoped session prompt and set of permissions. Agents
// contains the names of non-deleted agents assigned to the role; it is read-only.
type AgentRole struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Permissions []string `json:"permissions"`
	Default     bool     `json:"default"`
	Agents      []string `json:"agents"`
}

var (
	ErrInvalidAgentRole    = errors.New("invalid agent role")
	ErrAgentRoleDefault    = errors.New("the default agent role must remain assigned")
	ErrAgentRoleInUse      = errors.New("agent role is in use")
	ErrAgentRoleDelegation = errors.New("agent role requires administrator delegation")
)

// RequiresDelegation reports whether assigning this role requires administrator approval.
func (r AgentRole) RequiresDelegation() bool {
	return slices.ContainsFunc(r.Permissions, delegatedAgentPermission)
}

func delegatedAgentPermission(permission string) bool {
	switch permission {
	case "tail:swarm", "add", "start", "stop", "restart", "slash", "reset", "rm":
		return true
	default:
		return false
	}
}

func (r AgentRole) validate() error {
	if r.Name == "" {
		return fmt.Errorf("%w: enter a name for the role", ErrInvalidAgentRole)
	}
	if r.Default && r.RequiresDelegation() {
		return fmt.Errorf("%w: remove delegated permissions before making %q the default role", ErrInvalidAgentRole, r.Name)
	}
	seen := make(map[string]bool, len(r.Permissions))
	for _, permission := range r.Permissions {
		switch permission {
		case "agents", "diagnose", "send:agents", "send:operator", "tail:self":
		default:
			if !delegatedAgentPermission(permission) {
				return fmt.Errorf("%w: remove unknown permission %q", ErrInvalidAgentRole, permission)
			}
		}
		if seen[permission] {
			return fmt.Errorf("%w: remove duplicate permission %q", ErrInvalidAgentRole, permission)
		}
		seen[permission] = true
	}
	if seen["tail:self"] && seen["tail:swarm"] {
		return fmt.Errorf("%w: choose one terminal access scope", ErrInvalidAgentRole)
	}
	return nil
}

const agentRoleCols = `id,name,prompt,permissions,is_default`
const agentRoleAgents = `(SELECT json_group_array(name ORDER BY name,uid) FROM managed_actors WHERE live_role_id=agent_roles.id)`

func scanAgentRole(row interface{ Scan(...any) error }, includeAgents bool) (AgentRole, error) {
	var role AgentRole
	var permissions, agents string
	fields := []any{&role.ID, &role.Name, &role.Prompt, &permissions, &role.Default}
	if includeAgents {
		fields = append(fields, &agents)
	}
	if err := row.Scan(fields...); err != nil {
		return AgentRole{}, err
	}
	if err := json.Unmarshal([]byte(permissions), &role.Permissions); err != nil {
		return AgentRole{}, err
	}
	if includeAgents {
		if err := json.Unmarshal([]byte(agents), &role.Agents); err != nil {
			return AgentRole{}, err
		}
	}
	return role, nil
}

// GetAgentRole returns one role of the project. An empty roleID selects its default.
func (s *Store) GetAgentRole(ctx context.Context, projectID, roleID string) (AgentRole, error) {
	return s.getAgentRole(ctx, projectID, roleID, true)
}

func (s *Store) getAgentRole(ctx context.Context, projectID, roleID string, includeAgents bool) (AgentRole, error) {
	cols := agentRoleCols
	if includeAgents {
		cols += "," + agentRoleAgents
	}
	query := `SELECT ` + cols + ` FROM agent_roles WHERE project_id=?`
	var row *sql.Row
	if roleID == "" {
		row = s.conn(ctx).QueryRowContext(ctx, query+` AND is_default=1`, projectID)
	} else {
		row = s.conn(ctx).QueryRowContext(ctx, query+` AND id=?`, projectID, roleID)
	}
	role, err := scanAgentRole(row, includeAgents)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentRole{}, fmt.Errorf("%w: choose an agent role from this project", ErrNotFound)
	}
	return role, err
}

// ListAgentRoles lists the roles of the project by name.
func (s *Store) ListAgentRoles(ctx context.Context, projectID string) ([]AgentRole, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT `+agentRoleCols+`,`+agentRoleAgents+` FROM agent_roles WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := []AgentRole{}
	for rows.Next() {
		role, err := scanAgentRole(rows, true)
		if err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

// CreateAgentRole inserts a role with a generated ID. A new default replaces the old one.
func (s *Store) CreateAgentRole(ctx context.Context, projectID string, role AgentRole) (AgentRole, error) {
	role.Name = strings.TrimSpace(role.Name)
	if err := role.validate(); err != nil {
		return AgentRole{}, err
	}
	role.ID = NewID()
	role.Agents = []string{}
	role.Permissions = append([]string{}, role.Permissions...)
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx,
			`INSERT INTO agent_roles (id,project_id,name,prompt,permissions,is_default) VALUES (?,?,?,?,?,0)`,
			role.ID, projectID, role.Name, role.Prompt, encodeManaged(role.Permissions))
		if err != nil {
			return wrapUnique(err, fmt.Sprintf("agent role %q", role.Name))
		}
		if role.Default {
			return s.setDefaultAgentRole(ctx, projectID, role.ID)
		}
		return nil
	})
	if err != nil {
		return AgentRole{}, err
	}
	return role, nil
}

// UpdateAgentRole replaces editable fields without ever granting delegated
// authority to live agents that were created without administrator approval.
func (s *Store) UpdateAgentRole(ctx context.Context, projectID string, role AgentRole) error {
	if role.ID == "" {
		return fmt.Errorf("%w: select a role to update", ErrInvalidAgentRole)
	}
	role.Name = strings.TrimSpace(role.Name)
	if err := role.validate(); err != nil {
		return err
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.getAgentRole(ctx, projectID, role.ID, false)
		if err != nil {
			return err
		}
		if old.Default && !role.Default {
			return fmt.Errorf("%w; make another role the default first", ErrAgentRoleDefault)
		}
		if role.RequiresDelegation() {
			var agent string
			err := s.conn(ctx).QueryRowContext(ctx, `SELECT name FROM managed_actors
				WHERE live_role_id=? AND delegator_id='' ORDER BY name LIMIT 1`, role.ID).Scan(&agent)
			if err == nil {
				return fmt.Errorf("%w; agent %q has no administrator delegation. Remove delegated permissions or create a separate role", ErrAgentRoleDelegation, agent)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE agent_roles SET name=?,prompt=?,permissions=? WHERE project_id=? AND id=?`,
			role.Name, role.Prompt, encodeManaged(append([]string{}, role.Permissions...)), projectID, role.ID)
		if err != nil {
			return wrapUnique(err, fmt.Sprintf("agent role %q", role.Name))
		}
		if role.Default && !old.Default {
			return s.setDefaultAgentRole(ctx, projectID, role.ID)
		}
		return nil
	})
}

// setDefaultAgentRole runs in the same write transaction as the role change.
func (s *Store) setDefaultAgentRole(ctx context.Context, projectID, roleID string) error {
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE agent_roles SET is_default=0 WHERE project_id=? AND is_default=1`, projectID); err != nil {
		return err
	}
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE agent_roles SET is_default=1 WHERE project_id=? AND id=?`, projectID, roleID)
	return err
}

// DeleteAgentRole removes a non-default role with no live assignments. Agent
// tombstones retain their original role IDs without blocking deletion.
func (s *Store) DeleteAgentRole(ctx context.Context, projectID, roleID string) error {
	if roleID == "" {
		return fmt.Errorf("%w: select a role to delete", ErrInvalidAgentRole)
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		role, err := s.GetAgentRole(ctx, projectID, roleID)
		if err != nil {
			return err
		}
		if role.Default {
			return fmt.Errorf("%w; make another role the default before deleting %q", ErrAgentRoleDefault, role.Name)
		}
		if len(role.Agents) != 0 {
			return fmt.Errorf("%w: %q is assigned to %s; remove those agents first", ErrAgentRoleInUse, role.Name, strings.Join(role.Agents, ", "))
		}
		_, err = s.conn(ctx).ExecContext(ctx, `DELETE FROM agent_roles WHERE project_id=? AND id=?`, projectID, roleID)
		return err
	})
}
