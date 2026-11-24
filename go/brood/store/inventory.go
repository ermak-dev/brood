package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
)

var (
	ErrRevisionConflict = errors.New("the agent changed while the request was processed; reload and retry")
	ErrPoolUnavailable  = errors.New("the pool is not enabled; an administrator must enable it on the Pools page")
	ErrPoolCapacity     = errors.New("the pool has no free capacity for another agent; delete an agent or ask an administrator to raise the pool capacity")
	ErrImmutableBinding = errors.New("allocation binding is immutable")
	ErrInUse            = errors.New("the pool still has agents; delete them first")
)

// SourceSnapshot is public connection identity, never authentication material.
// ClusterIdentity must identify the PVE cluster across all connection aliases.
// CAFingerprint is a public trust fingerprint, not a certificate or private key.
// Callers resolve credentials separately from Connection at execution time.
type SourceSnapshot struct {
	ClusterIdentity string
	Endpoint        string
	Node            string
	ResourcePool    string
	Storage         string
	Network         string
	CAFingerprint   string
}

// Resources expresses pool ceilings or an agent's reserved resources.
// Zero pool capacity is allowed and prevents new reservations.
type Resources struct {
	CPUMillicores int64
	MemoryBytes   int64
	DiskBytes     int64
}

// TemplateSnapshot is the immutable definition of a profile template.
// Profile identifies a trusted, deterministic provider definition, not guest input.
type TemplateSnapshot struct {
	Profile string
	URI     string
	Digest  string
}

// Pool is a Proxmox pool activated from a connection.
// State is enabled, draining, or deleted (a retained tombstone).
// Resources is the total capacity shared across all projects.
type Pool struct {
	UID               string
	Name              string
	Connection        string
	Source            SourceSnapshot
	SourceFingerprint string
	Labels            map[string]string
	MaxActors         int64
	Resources         Resources
	Template          TemplateSnapshot
	State             string
	Revision          int64
	CreatedAt         string
	UpdatedAt         string
}

// Agent keeps allocation identity and the current durable operation intent.
// All phases except deleted consume capacity. Deleted rows are retained for audit.
// Error must be a sanitized public diagnostic; never pass raw provider responses.
// It may begin with an unsent mark (Unsent); callers show Diagnostic.
// OperationID is caller-generated and must be fresh for each new intent.
// Role is the ID of the assigned global role. DelegatorID records human
// approval independently of current role permissions. ParentUID records the
// creating agent. These bindings are immutable.
// Origin is the canonical Git origin to clone, or empty for an empty
// workspace. It is immutable; its access mode is recomputed from current policy.
// Stage is create, start, stop, delete, or done. It fences task callbacks
// independently of observed Phase.
type Agent struct {
	Role            string
	DelegatorID     string
	ParentUID       string
	UID             string
	Name            string
	SwarmID         string
	PoolUID         string
	ClusterIdentity string
	Template        TemplateSnapshot
	Incarnation     int64
	Phase           string
	Desired         string
	Resources       Resources
	CTID            int
	OwnerNonce      string
	OperationID     string
	Operation       string
	Stage           string
	UPID            string
	Error           string
	Revision        int64
	CreatedAt       string
	UpdatedAt       string
	Origin          string
}

func validatePool(p Pool) error {
	if p.UID == "" || p.Name == "" || p.Connection == "" || p.Source.ClusterIdentity == "" || p.Source.Node == "" || p.Source.ResourcePool == "" || p.Source.Storage == "" || p.Source.Network == "" {
		return fmt.Errorf("pool identity and public connection fields are required")
	}
	endpoint, err := url.Parse(p.Source.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return fmt.Errorf("pool source requires a public HTTPS endpoint without credentials or query")
	}
	if p.MaxActors < 0 || !nonnegativeResources(p.Resources) {
		return fmt.Errorf("pool capacity must be nonnegative")
	}
	if p.State != "enabled" && p.State != "draining" {
		return fmt.Errorf("pool state must be enabled or draining")
	}
	if p.Template.Profile == "" || p.Template.URI == "" || !strings.HasPrefix(p.Template.Digest, "sha256:") {
		return fmt.Errorf("template profile, URI and sha256 digest are required")
	}
	if !validDigest(p.Template.Digest) {
		return fmt.Errorf("template digest must be sha256")
	}
	if p.Source.CAFingerprint != "" && !validDigest(p.Source.CAFingerprint) {
		return fmt.Errorf("CA fingerprint must be sha256")
	}
	templateURI, err := url.Parse(p.Template.URI)
	if err != nil || templateURI.User != nil || templateURI.RawQuery != "" || templateURI.Fragment != "" {
		return fmt.Errorf("template URI must not contain credentials or query")
	}
	return nil
}
func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(digest) == 32
}

func nonnegativeResources(r Resources) bool {
	return r.CPUMillicores >= 0 && r.MemoryBytes >= 0 && r.DiskBytes >= 0
}
func sourceFingerprint(source SourceSnapshot) string {
	data, _ := json.Marshal(source)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func encodeJSON(value any) string { data, _ := json.Marshal(value); return string(data) }

const poolCols = `uid,name,connection,source,source_fingerprint,labels,max_actors,cpu_millicores,memory_bytes,disk_bytes,template,state,revision,created_at,updated_at`

func scanPool(row interface{ Scan(...any) error }) (Pool, error) {
	var p Pool
	var source, labels, template string
	err := row.Scan(&p.UID, &p.Name, &p.Connection, &source, &p.SourceFingerprint, &labels, &p.MaxActors, &p.Resources.CPUMillicores, &p.Resources.MemoryBytes, &p.Resources.DiskBytes, &template, &p.State, &p.Revision, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	for _, item := range []struct {
		raw    string
		target any
	}{{source, &p.Source}, {labels, &p.Labels}, {template, &p.Template}} {
		if err := json.Unmarshal([]byte(item.raw), item.target); err != nil {
			return Pool{}, fmt.Errorf("invalid stored pool snapshot: %w", err)
		}
	}
	return p, nil
}

// CreatePool activates public provider configuration with revision one.
func (s *Store) CreatePool(ctx context.Context, p Pool) (Pool, error) {
	if err := validatePool(p); err != nil {
		return Pool{}, err
	}
	p.SourceFingerprint = sourceFingerprint(p.Source)
	p.Revision = 1
	p.CreatedAt = nowRFC3339()
	p.UpdatedAt = p.CreatedAt
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO managed_pools (`+poolCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.UID, p.Name, p.Connection, encodeJSON(p.Source), p.SourceFingerprint, encodeJSON(p.Labels), p.MaxActors, p.Resources.CPUMillicores, p.Resources.MemoryBytes, p.Resources.DiskBytes, encodeJSON(p.Template), p.State, p.Revision, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return Pool{}, wrapUnique(err, "pool")
	}
	return p, nil
}

// GetPool returns a pool including deleted tombstones.
func (s *Store) GetPool(ctx context.Context, uid string) (Pool, error) {
	p, err := scanPool(s.conn(ctx).QueryRowContext(ctx, `SELECT `+poolCols+` FROM managed_pools WHERE uid=?`, uid))
	if err == sql.ErrNoRows {
		err = fmt.Errorf("%w: pool", ErrNotFound)
	}
	return p, err
}

// ListPools returns all pools, including tombstones, in UID order.
func (s *Store) ListPools(ctx context.Context) ([]Pool, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT `+poolCols+` FROM managed_pools ORDER BY uid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pools := []Pool{}
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		pools = append(pools, p)
	}
	return pools, rows.Err()
}

// UpdatePool replaces mutable configuration with optimistic concurrency.
// Connection and template bindings cannot change while allocations exist.
// Capacity cannot shrink below current reservations; draining permits stop/delete.
func (s *Store) UpdatePool(ctx context.Context, p Pool, expected int64) (Pool, error) {
	var result Pool
	err := s.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.GetPool(ctx, p.UID)
		if err != nil {
			return err
		}
		if old.Revision != expected {
			return ErrRevisionConflict
		}
		if old.State == "deleted" {
			return ErrPoolUnavailable
		}
		if err := validatePool(p); err != nil {
			return err
		}
		count, used, err := s.poolUsage(ctx, p.UID)
		if err != nil {
			return err
		}
		if count > 0 && (old.Connection != p.Connection || old.Source != p.Source || old.Template != p.Template) {
			return ErrImmutableBinding
		}
		if !poolFits(p, count, used, Resources{}) {
			return ErrPoolCapacity
		}
		p.SourceFingerprint = sourceFingerprint(p.Source)
		p.CreatedAt = old.CreatedAt
		p.UpdatedAt = nowRFC3339()
		p.Revision = expected + 1
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_pools SET name=?,connection=?,source=?,source_fingerprint=?,labels=?,max_actors=?,cpu_millicores=?,memory_bytes=?,disk_bytes=?,template=?,state=?,revision=?,updated_at=? WHERE uid=?`, p.Name, p.Connection, encodeJSON(p.Source), p.SourceFingerprint, encodeJSON(p.Labels), p.MaxActors, p.Resources.CPUMillicores, p.Resources.MemoryBytes, p.Resources.DiskBytes, encodeJSON(p.Template), p.State, p.Revision, p.UpdatedAt, p.UID)
		result = p
		return wrapUnique(err, "pool")
	})
	return result, err
}

// DeletePool retains an audit tombstone after checking all live allocations.
func (s *Store) DeletePool(ctx context.Context, uid string, expected int64) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.GetPool(ctx, uid)
		if err != nil {
			return err
		}
		if p.Revision != expected {
			return ErrRevisionConflict
		}
		count, _, err := s.poolUsage(ctx, uid)
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrInUse
		}
		if p.State == "deleted" {
			return nil
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_pools SET state='deleted',revision=revision+1,updated_at=? WHERE uid=?`, nowRFC3339(), uid)
		return err
	})
}

func (s *Store) poolUsage(ctx context.Context, uid string) (int64, Resources, error) {
	var count int64
	var used Resources
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*),coalesce(sum(cpu_millicores),0),coalesce(sum(memory_bytes),0),coalesce(sum(disk_bytes),0) FROM agents WHERE pool_uid=? AND phase<>'deleted'`, uid).Scan(&count, &used.CPUMillicores, &used.MemoryBytes, &used.DiskBytes)
	return count, used, err
}
func poolFits(p Pool, count int64, used, extra Resources) bool {
	return count <= p.MaxActors && used.CPUMillicores <= p.Resources.CPUMillicores && extra.CPUMillicores <= p.Resources.CPUMillicores-used.CPUMillicores && used.MemoryBytes <= p.Resources.MemoryBytes && extra.MemoryBytes <= p.Resources.MemoryBytes-used.MemoryBytes && used.DiskBytes <= p.Resources.DiskBytes && extra.DiskBytes <= p.Resources.DiskBytes-used.DiskBytes
}

const agentCols = `uid,name,swarm_id,pool_uid,cluster_identity,template,incarnation,phase,desired,cpu_millicores,memory_bytes,disk_bytes,ctid,owner_nonce,operation_id,operation,upid,error,revision,created_at,updated_at,stage,role,delegator_id,parent_uid,origin`

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var template string
	err := row.Scan(&a.UID, &a.Name, &a.SwarmID, &a.PoolUID, &a.ClusterIdentity, &template, &a.Incarnation, &a.Phase, &a.Desired, &a.Resources.CPUMillicores, &a.Resources.MemoryBytes, &a.Resources.DiskBytes, &a.CTID, &a.OwnerNonce, &a.OperationID, &a.Operation, &a.UPID, &a.Error, &a.Revision, &a.CreatedAt, &a.UpdatedAt, &a.Stage, &a.Role, &a.DelegatorID, &a.ParentUID, &a.Origin)
	if err == nil {
		err = json.Unmarshal([]byte(template), &a.Template)
	}
	return a, err
}

// ReserveAgent atomically reserves live pool capacity across projects.
// Input needs UID, Name, SwarmID, PoolUID, Resources, CTID, OwnerNonce and a
// create OperationID. The store snapshots all binding fields.
// The create intent must commit before the caller writes to PVE. When nested in
// WithTx, callers must wait for the outer transaction to commit before any write.
func (s *Store) ReserveAgent(ctx context.Context, a Agent) (Agent, error) {
	if a.UID == "" || a.Name == "" || a.SwarmID == "" || a.PoolUID == "" || a.OwnerNonce == "" || a.OperationID == "" || a.CTID <= 0 || a.Resources.CPUMillicores <= 0 || a.Resources.MemoryBytes <= 0 || a.Resources.DiskBytes <= 0 {
		return Agent{}, fmt.Errorf("agent identity, ownership, operation and positive resources are required")
	}
	if !storedOrigin(a.Origin) {
		return Agent{}, fmt.Errorf("agent origin must be empty or a canonical HTTPS or SSH origin")
	}
	var result Agent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.GetPool(ctx, a.PoolUID)
		if err != nil {
			return err
		}
		if p.State != "enabled" {
			return ErrPoolUnavailable
		}
		sw, err := s.GetSwarm(ctx, a.SwarmID)
		if err != nil {
			return err
		}
		if sw.State != "active" {
			return fmt.Errorf("agent requires an active swarm")
		}
		role, err := s.getAgentRole(ctx, a.Role, false)
		if err != nil {
			return err
		}
		if a.ParentUID != "" && (!role.Default || a.DelegatorID != "") {
			return fmt.Errorf("%w: an agent can create children only with the default role and no human delegation", ErrInvalidAgentRole)
		}
		if role.RequiresDelegation() && a.DelegatorID == "" {
			return fmt.Errorf("%w: ask an administrator to assign %q", ErrAgentRoleDelegation, role.Name)
		}
		if a.DelegatorID != "" {
			delegator, err := s.GetUser(ctx, a.DelegatorID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err != nil || delegator.Disabled || !delegator.IsAdmin {
				return fmt.Errorf("%w: ask an active administrator to assign %q", ErrAgentRoleDelegation, role.Name)
			}
		}
		a.Role = role.ID
		count, used, err := s.poolUsage(ctx, p.UID)
		if err != nil {
			return err
		}
		if count >= p.MaxActors || !poolFits(p, count, used, a.Resources) {
			return ErrPoolCapacity
		}
		a.ClusterIdentity = p.Source.ClusterIdentity
		// Клон образа проекта называет образ сам; иначе базовый шаблон пула.
		if _, image := ProjectImageFromURI(a.Template.URI); !image {
			a.Template = p.Template
		}
		a.Incarnation = 1
		a.Phase = "provisioning"
		a.Desired = "running"
		a.Operation = "create"
		a.Stage = "create"
		a.UPID = ""
		a.Error = ""
		a.Revision = 1
		a.CreatedAt = nowRFC3339()
		a.UpdatedAt = a.CreatedAt
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.UID, a.Name, a.SwarmID, a.PoolUID, a.ClusterIdentity, encodeJSON(a.Template), a.Incarnation, a.Phase, a.Desired, a.Resources.CPUMillicores, a.Resources.MemoryBytes, a.Resources.DiskBytes, a.CTID, a.OwnerNonce, a.OperationID, a.Operation, a.UPID, a.Error, a.Revision, a.CreatedAt, a.UpdatedAt, a.Stage, a.Role, a.DelegatorID, a.ParentUID, a.Origin)
		if err != nil {
			return wrapUnique(err, "agent name or CTID")
		}
		result = a
		return nil
	})
	return result, err
}

// GetAgent returns immutable identity, intent and latest reconciliation state.
func (s *Store) GetAgent(ctx context.Context, uid string) (Agent, error) {
	a, err := scanAgent(s.conn(ctx).QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE uid=?`, uid))
	if err == sql.ErrNoRows {
		err = fmt.Errorf("%w: agent", ErrNotFound)
	}
	return a, err
}

// ListAgents lists all rows for reconciliation; an empty swarmID means all swarms.
// Deleted tombstones are included so callers can distinguish absent from retired UIDs.
func (s *Store) ListAgents(ctx context.Context, swarmID string) ([]Agent, error) {
	query := `SELECT ` + agentCols + ` FROM agents`
	args := []any{}
	if swarmID != "" {
		query += ` WHERE swarm_id=?`
		args = append(args, swarmID)
	}
	query += ` ORDER BY uid`
	rows, err := s.conn(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// SwarmHasAgents reports live agents of the swarm, including provisioning,
// stopped and failed ones.
// Callers must check this before swarm deletion under their admission lock.
func (s *Store) SwarmHasAgents(ctx context.Context, swarmID string) (bool, error) {
	var exists bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE swarm_id=? AND phase<>'deleted')`, swarmID).Scan(&exists)
	return exists, err
}

// UpdateAgent CAS-updates only observed Phase, UPID and sanitized Error.
// It cannot release capacity, change ownership, or change operation intent/stage.
// Phase is diagnostic, not task completion. Prefer BindAgentTask for UPID;
// this compatibility path can bind only an active, previously unbound task slot.
// Error keeps or drops the row's unsent mark (Agent.Unsent) but never
// sets one: only stage transitions say that a write was not submitted.
func (s *Store) UpdateAgent(ctx context.Context, a Agent, expected int64) (Agent, error) {
	var result Agent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.GetAgent(ctx, a.UID)
		if err != nil {
			return err
		}
		if old.Revision != expected {
			return ErrRevisionConflict
		}
		comparison := a
		comparison.Phase = old.Phase
		comparison.UPID = old.UPID
		comparison.Error = old.Error
		comparison.Revision = old.Revision
		comparison.UpdatedAt = old.UpdatedAt
		if !reflect.DeepEqual(comparison, old) {
			return ErrImmutableBinding
		}
		if old.Phase == "deleted" || a.Phase == "deleted" {
			return fmt.Errorf("deletion requires explicit confirmation")
		}
		if a.Phase != "provisioning" && a.Phase != "running" && a.Phase != "stopped" && a.Phase != "stopping" && a.Phase != "error" && a.Phase != "deleting" {
			return fmt.Errorf("invalid agent phase")
		}
		if (old.UPID != "" || old.Stage == "done") && a.UPID != old.UPID {
			return fmt.Errorf("task is already bound or requires stage reconciliation")
		}
		if mark := a.Unsent(); mark != "" && mark != old.Unsent() {
			return fmt.Errorf("unsent mark is set only by a stage transition")
		}
		a.Revision = expected + 1
		a.UpdatedAt = nowRFC3339()
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE agents SET phase=?,upid=?,error=?,revision=?,updated_at=? WHERE uid=?`, a.Phase, a.UPID, a.Error, a.Revision, a.UpdatedAt, a.UID)
		result = a
		return err
	})
	return result, err
}

// BeginAgentOperation commits intent before any external mutation.
// start/reset require an enabled pool; stop/delete remain legal while draining.
// A matching current ID/kind is idempotent. Unknown outcomes must be reconciled
// before beginning any different operation, including cleanup. Observed Phase alone
// does not reconcile a task: use ConfirmAgentStage or the explicit terminal
// failure API first. Stage=done permits a reconciled error to begin fresh work.
// Reset is destructive and retains old ownership until confirmed old absence.
func (s *Store) BeginAgentOperation(ctx context.Context, uid string, expected int64, operationID, operation string) (Agent, error) {
	return s.beginAgentOperation(ctx, uid, expected, operationID, operation, false)
}

func (s *Store) beginAgentOperation(ctx context.Context, uid string, expected int64, operationID, operation string, owner bool) (Agent, error) {
	var result Agent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetAgent(ctx, uid)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrRevisionConflict
		}
		if operationID == "" || a.Phase == "deleted" {
			return fmt.Errorf("active agent and operation ID are required")
		}
		if a.Desired == "deleted" && operation != "delete" {
			return fmt.Errorf("deletion intent cannot be canceled")
		}
		if operationID == a.OperationID {
			if operation != a.Operation {
				return fmt.Errorf("operation ID is already bound")
			}
			result = a
			return nil
		}
		if !owner && (operation == "start" || operation == "stop" || operation == "delete" || operation == "reset") {
			if err := s.guardActorOperation(ctx, a); err != nil {
				return err
			}
		}
		if a.Stage != "done" {
			return fmt.Errorf("operation task must be reconciled before replacement")
		}
		desired := ""
		switch operation {
		case "start", "reset":
			desired = "running"
			p, err := s.GetPool(ctx, a.PoolUID)
			if err != nil {
				return err
			}
			if p.State != "enabled" {
				return ErrPoolUnavailable
			}
			if a.Phase != "running" && a.Phase != "stopped" && a.Phase != "error" {
				return fmt.Errorf("agent must be reconciled before start or reset")
			}
		case "stop":
			if a.Phase != "running" && a.Phase != "stopped" && a.Phase != "error" {
				return fmt.Errorf("agent must be reconciled before stop")
			}
			desired = "stopped"
		case "delete":
			desired = "deleted"
		default:
			return fmt.Errorf("invalid agent operation")
		}
		a.Stage = operation
		if operation == "reset" || operation == "delete" {
			a.Stage = "stop"
		}
		a.OperationID = operationID
		a.Operation = operation
		a.Desired = desired
		a.UPID = ""
		a.Error = ""
		a.Revision++
		a.UpdatedAt = nowRFC3339()
		switch operation {
		case "delete":
			a.Phase = "deleting"
			a.Error = StopUnsent
		case "stop", "reset":
			a.Phase = "stopping"
			a.Error = StopUnsent
		case "start":
			a.Phase = "provisioning"
		}
		updated, err := s.conn(ctx).ExecContext(ctx, `UPDATE agents SET operation_id=?,operation=?,desired=?,upid='',error=?,revision=?,updated_at=?,incarnation=?,phase=?,stage=? WHERE uid=?`, a.OperationID, a.Operation, a.Desired, a.Error, a.Revision, a.UpdatedAt, a.Incarnation, a.Phase, a.Stage, uid)
		if err != nil {
			return err
		}
		if n, err := updated.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrRevisionConflict
		}
		result = a
		return nil
	})
	return result, err
}

// ConfirmAgentDeleted releases capacity only after caller-confirmed PVE absence.
// Caller must verify owned CT absence AND terminal delete task reconciliation,
// including unknown submissions. This method performs no external calls and
// requires the completed stop stage; absence alone cannot bypass a pending stop.
// OperationID and revision fence stale cleanup observations from newer operations.
// The tombstone keeps the public diagnostic but no unsent mark (Unsent).
func (s *Store) ConfirmAgentDeleted(ctx context.Context, uid string, expected int64, operationID string) (Agent, error) {
	var result Agent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetAgent(ctx, uid)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrRevisionConflict
		}
		if a.Operation != "delete" || a.Desired != "deleted" || a.OperationID != operationID {
			return fmt.Errorf("deletion intent does not match confirmation")
		}
		if a.Phase == "deleted" {
			result = a
			return nil
		}
		if a.Stage != "delete" {
			return fmt.Errorf("stop stage must be confirmed before deletion")
		}
		a.Phase = "deleted"
		a.Stage = "done"
		// Удалённая строка — не ждущая стадия: отметка незапрошенного Delete
		// (#277) снимается, диагностика последнего отказа остаётся.
		a.Error = a.Diagnostic()
		a.Revision++
		a.UpdatedAt = nowRFC3339()
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE agents SET phase='deleted',stage='done',error=?,revision=?,updated_at=? WHERE uid=?`, a.Error, a.Revision, a.UpdatedAt, uid)
		result = a
		return err
	})
	return result, err
}

// BindAgentTask records a submitted task for exactly one operation stage.
// Commit the intent before submission, then bind its UPID before polling. A lost
// submission response must be reconciled externally, never blindly resubmitted.
func (s *Store) BindAgentTask(ctx context.Context, uid string, expected int64, operationID, stage, upid string) (Agent, error) {
	return s.changeAgentStage(ctx, uid, expected, operationID, stage, func(a *Agent) error {
		if upid == "" || stage == "done" {
			return fmt.Errorf("active task stage and nonempty UPID are required")
		}
		if a.UPID != "" && a.UPID != upid {
			return fmt.Errorf("operation UPID is already bound")
		}
		a.UPID = upid
		return nil
	})
}

// ConfirmAgentStage records caller-confirmed terminal task reconciliation
// AND owned CT state (running, stopped, or absent). Absence alone is insufficient
// while a task may still create/start the CT. No external calls are made here.
// The caller must reconcile an unknown submission even when UPID is empty.
// Cleanup stop advances to delete with a fresh task slot; normal work becomes
// done without discarding its task evidence. Phase updates alone never do this.
// A cleanup stop confirmed stopped
// advances to a delete stage marked DeleteUnsent; a confirmed absence
// leaves no mark, and the caller releases it in the same transaction.
func (s *Store) ConfirmAgentStage(ctx context.Context, uid string, expected int64, operationID, stage, observed string) (Agent, error) {
	return s.changeAgentStage(ctx, uid, expected, operationID, stage, func(a *Agent) error {
		if observed != "running" && observed != "stopped" && observed != "absent" {
			return fmt.Errorf("confirmed CT state must be running, stopped, or absent")
		}
		if stage == "done" || stage == "delete" {
			return fmt.Errorf("stage requires deletion or reset confirmation")
		}
		cleanup := (a.Operation == "delete" || a.Operation == "reset") && stage == "stop"
		if stage == "stop" && observed == "running" {
			return fmt.Errorf("stop requires confirmed stopped or absent CT")
		}
		if cleanup {
			if observed == "running" {
				a.Stage = "stop"
			} else {
				a.Stage = "delete"
			}
			a.UPID = ""
		} else {
			a.Stage = "done"
		}
		a.Phase = observed
		if observed == "absent" {
			a.Phase = "error"
		}
		a.Error = ""
		if cleanup && observed == "stopped" {
			a.Error = DeleteUnsent
		}
		return nil
	})
}

// ResolveAgentTaskFailure records caller-confirmed terminal task failure
// AND an ownership-verified running/stopped CT or globally confirmed absence.
// The caller must verify the exact bound UPID is terminal and failed; observed
// state alone never proves this. Unknown outcomes and unbound submissions must
// not use this API. No external calls are made here.
// Revision, operation ID, stage and failed UPID fence the assertion. The slot
// becomes done with diagnostic Phase=error, without claiming the target state
// was achieved. Task/error evidence, intent, ownership and capacity are retained.
// Retry requires a fresh operation ID; cleanup retries begin again at stop.
func (s *Store) ResolveAgentTaskFailure(ctx context.Context, uid string, expected int64, operationID, stage, failedUPID, observed string) (Agent, error) {
	return s.changeAgentStage(ctx, uid, expected, operationID, stage, func(a *Agent) error {
		if stage != "create" && stage != "start" && stage != "stop" && stage != "delete" {
			return fmt.Errorf("active task stage is required for failure resolution")
		}
		if failedUPID == "" || a.UPID != failedUPID {
			return fmt.Errorf("failed task UPID must match the bound task")
		}
		if observed != "running" && observed != "stopped" && observed != "absent" {
			return fmt.Errorf("confirmed CT state must be running, stopped, or absent")
		}
		a.Stage = "done"
		a.Phase = "error"
		return nil
	})
}

// ConfirmAgentReset confirms the old owned CT is absent AND its delete
// task is terminal, then atomically creates a fresh incarnation/create intent.
// Reset destroys the workspace; stop/start retain it. UID, CTID, resource and
// binding reservations are retained continuously, including while errors persist.
// The caller supplies a fresh unpredictable owner nonce, different from the old.
// Operation remains reset; Stage=create identifies its new create task slot.
func (s *Store) ConfirmAgentReset(ctx context.Context, uid string, expected int64, operationID, freshNonce string) (Agent, error) {
	var result Agent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		next, err := s.changeAgentStage(ctx, uid, expected, operationID, "delete", func(a *Agent) error {
			if a.Operation != "reset" || a.Desired != "running" || freshNonce == "" || freshNonce == a.OwnerNonce {
				return fmt.Errorf("reset intent and fresh ownership nonce are required")
			}
			a.Incarnation++
			a.OwnerNonce = freshNonce
			a.Stage = "create"
			a.Phase = "provisioning"
			a.UPID = ""
			a.Error = ""
			return nil
		})
		if err != nil {
			return err
		}
		// Игнорированный UPDATE не должен создавать исторический факт.
		current, err := s.GetAgent(ctx, uid)
		if err != nil {
			return err
		}
		if current.Revision != next.Revision {
			return ErrRevisionConflict
		}
		result = next
		return nil
	})
	return result, err
}

func (s *Store) changeAgentStage(ctx context.Context, uid string, expected int64, operationID, stage string, change func(*Agent) error) (Agent, error) {
	var result Agent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetAgent(ctx, uid)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrRevisionConflict
		}
		if operationID == "" || a.OperationID != operationID || a.Stage != stage || a.Phase == "deleted" {
			return fmt.Errorf("operation stage does not match confirmation")
		}
		if err := change(&a); err != nil {
			return err
		}
		a.Revision++
		a.UpdatedAt = nowRFC3339()
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE agents SET stage=?,phase=?,upid=?,error=?,incarnation=?,owner_nonce=?,revision=?,updated_at=? WHERE uid=?`, a.Stage, a.Phase, a.UPID, a.Error, a.Incarnation, a.OwnerNonce, a.Revision, a.UpdatedAt, uid)
		result = a
		return err
	})
	return result, err
}

// Unsent marks (#277) begin Error of a stop or delete stage whose PVE write
// has not been requested; the public diagnostic of the last failure, if any,
// follows after ": ". BeginAgentOperation marks every stop stage it
// creates, ConfirmAgentStage marks the delete stage of a cleanup, and
// MarkAgentDeleteUnsent marks a delete stage again after a Delete
// refused before any write. UpdateAgent never sets a mark, and
// ConfirmAgentDeleted drops it from the tombstone. A stage without a
// mark may have been submitted, including every stage written before #277.
const (
	// StopUnsent marks a stop stage created before any Stop request.
	StopUnsent = "stop not requested"
	// DeleteUnsent marks a delete stage whose Delete is not requested.
	DeleteUnsent = "delete not requested"
)

// Unsent returns the unsent mark that begins a.Error, or "" without one.
func (a Agent) Unsent() string {
	mark, _ := splitUnsent(a.Error)
	return mark
}

// Diagnostic returns the public diagnostic of a.Error without its unsent mark.
// Callers show this text: the mark is state, not a failure.
func (a Agent) Diagnostic() string {
	_, diagnostic := splitUnsent(a.Error)
	return diagnostic
}

// JoinUnsent joins an unsent mark ("" for none) with a diagnostic.
func JoinUnsent(mark, diagnostic string) string {
	switch {
	case mark == "":
		return diagnostic
	case diagnostic == "":
		return mark
	}
	return mark + ": " + diagnostic
}

func splitUnsent(text string) (mark, diagnostic string) {
	for _, mark := range []string{StopUnsent, DeleteUnsent} {
		if text == mark {
			return mark, ""
		}
		if rest, ok := strings.CutPrefix(text, mark+": "); ok {
			return mark, rest
		}
	}
	return "", text
}

// MarkAgentDeleteUnsent records that the Delete of a delete stage
// failed before any PVE write (a read of ownership or an explicit refusal):
// the stage waits with Phase=stopped, DeleteUnsent and diagnostic, and
// the caller may submit it again. Only a failure that proves nothing was
// submitted may call this; a lost reply or an unrecorded UPID must not.
func (s *Store) MarkAgentDeleteUnsent(ctx context.Context, uid string, expected int64, operationID, diagnostic string) (Agent, error) {
	return s.changeAgentStage(ctx, uid, expected, operationID, "delete", func(a *Agent) error {
		if a.UPID != "" || (a.Operation != "delete" && a.Operation != "reset") {
			return fmt.Errorf("delete stage without a bound task is required")
		}
		a.Phase = "stopped"
		a.Error = JoinUnsent(DeleteUnsent, diagnostic)
		return nil
	})
}

// ReopenAgentStop returns a delete stage whose Delete was never
// requested (DeleteUnsent) to the stop stage of the same operation
// after the caller observed its owned CT running: it was started outside
// brood. The stop waits with Phase=running and no task, as after a Stop
// refused before any write (#275). A delete stage without the mark may have
// been submitted and is refused.
func (s *Store) ReopenAgentStop(ctx context.Context, uid string, expected int64, operationID string) (Agent, error) {
	return s.changeAgentStage(ctx, uid, expected, operationID, "delete", func(a *Agent) error {
		if a.UPID != "" || a.Unsent() != DeleteUnsent || (a.Operation != "delete" && a.Operation != "reset") {
			return fmt.Errorf("delete stage must be unsent to return to its stop stage")
		}
		a.Stage = "stop"
		a.Phase = "running"
		a.Error = ""
		return nil
	})
}

// LookupAgent resolves an agent by its name within one swarm for
// swarm-scoped delivery. Deleted tombstones resolve to ErrNotFound.
func (s *Store) LookupAgent(ctx context.Context, swarmID, name string) (Agent, error) {
	a, err := scanAgent(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE swarm_id=? AND name=? AND phase<>'deleted'`, swarmID, name))
	if err == sql.ErrNoRows {
		return a, fmt.Errorf("%w: agent", ErrNotFound)
	}
	return a, err
}
