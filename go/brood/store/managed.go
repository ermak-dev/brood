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
	ErrRevisionConflict = errors.New("managed inventory revision conflict")
	ErrPoolUnavailable  = errors.New("managed pool is not enabled")
	ErrPoolCapacity     = errors.New("managed pool capacity exhausted")
	ErrManagedBinding   = errors.New("managed allocation binding is immutable")
	ErrManagedChildren  = errors.New("managed pool has allocated actors")
)

// ManagedSourceSnapshot is public connection identity, never authentication material.
// ClusterIdentity must identify the PVE cluster across all connection aliases.
// CAFingerprint is a public trust fingerprint, not a certificate or private key.
// Callers resolve credentials separately from Connection at execution time.
type ManagedSourceSnapshot struct {
	ClusterIdentity string
	Endpoint        string
	Node            string
	ResourcePool    string
	Storage         string
	Network         string
	CAFingerprint   string
}

// ManagedResources expresses pool ceilings or an actor's reserved resources.
// Zero pool capacity is allowed and prevents new reservations.
type ManagedResources struct {
	CPUMillicores int64
	MemoryBytes   int64
	DiskBytes     int64
}

// ManagedTemplateSnapshot is the immutable definition of a managed profile template.
// Profile identifies a trusted, deterministic provider definition, not guest input.
type ManagedTemplateSnapshot struct {
	Profile string
	URI     string
	Digest  string
}

// ManagedPool is durable provider inventory, not a native WorkerPool or Worker.
// State is enabled, draining, or deleted (a retained tombstone).
// Resources is the total capacity shared across all projects.
type ManagedPool struct {
	UID               string
	Name              string
	Connection        string
	Source            ManagedSourceSnapshot
	SourceFingerprint string
	Labels            map[string]string
	MaxActors         int64
	Resources         ManagedResources
	Template          ManagedTemplateSnapshot
	State             string
	Revision          int64
	CreatedAt         string
	UpdatedAt         string
}

// ManagedActor keeps allocation identity and the current durable operation intent.
// All phases except deleted consume capacity. Deleted rows are retained for audit.
// Error must be a sanitized public diagnostic; never pass raw provider responses.
// OperationID is caller-generated and must be fresh for each new intent.
// Role defaults to worker. A master retains its approving human's DelegatorID;
// delegated workers retain the creating master's ParentUID for idempotent retries.
// These bindings are immutable and never derived from names or guest environment.
// Stage is create, start, stop, delete, or done. It fences task callbacks
// independently of observed Phase. Migrated schema-3 rows start at reconcile:
// callers must settle their existing task before new work, even if Phase looks
// terminal. A legacy reset is refused because schema 3 rotated ownership early.
type ManagedActor struct {
	Role              string
	DelegatorID       string
	ParentUID         string
	UID               string
	Name              string
	SwarmID           string
	AtespaceUID       string
	PoolUID           string
	ClusterIdentity   string
	SourceFingerprint string
	Template          ManagedTemplateSnapshot
	Incarnation       int64
	Phase             string
	Desired           string
	Resources         ManagedResources
	CTID              int
	OwnerNonce        string
	OperationID       string
	Operation         string
	Stage             string
	UPID              string
	Error             string
	Revision          int64
	CreatedAt         string
	UpdatedAt         string
	DeletedAt         *string
}

func validateManagedPool(p ManagedPool) error {
	if p.UID == "" || p.Name == "" || p.Connection == "" || p.Source.ClusterIdentity == "" || p.Source.Node == "" || p.Source.ResourcePool == "" || p.Source.Storage == "" || p.Source.Network == "" {
		return fmt.Errorf("managed pool identity and public connection fields are required")
	}
	endpoint, err := url.Parse(p.Source.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return fmt.Errorf("managed source requires a public HTTPS endpoint without credentials or query")
	}
	if p.MaxActors < 0 || !nonnegativeResources(p.Resources) {
		return fmt.Errorf("managed pool capacity must be nonnegative")
	}
	if p.State != "enabled" && p.State != "draining" {
		return fmt.Errorf("managed pool state must be enabled or draining")
	}
	if p.Template.Profile == "" || p.Template.URI == "" || !strings.HasPrefix(p.Template.Digest, "sha256:") {
		return fmt.Errorf("managed template profile, URI and sha256 digest are required")
	}
	if !validManagedDigest(p.Template.Digest) {
		return fmt.Errorf("managed template digest must be sha256")
	}
	if p.Source.CAFingerprint != "" && !validManagedDigest(p.Source.CAFingerprint) {
		return fmt.Errorf("managed CA fingerprint must be sha256")
	}
	templateURI, err := url.Parse(p.Template.URI)
	if err != nil || templateURI.User != nil || templateURI.RawQuery != "" || templateURI.Fragment != "" {
		return fmt.Errorf("managed template URI must not contain credentials or query")
	}
	return validateCapacityLabels(p.Labels)
}
func validManagedDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(digest) == 32
}

func nonnegativeResources(r ManagedResources) bool {
	return r.CPUMillicores >= 0 && r.MemoryBytes >= 0 && r.DiskBytes >= 0
}
func sourceFingerprint(source ManagedSourceSnapshot) string {
	data, _ := json.Marshal(source)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func encodeManaged(value any) string { data, _ := json.Marshal(value); return string(data) }

const managedPoolCols = `uid,name,connection,source,source_fingerprint,labels,max_actors,cpu_millicores,memory_bytes,disk_bytes,template,state,revision,created_at,updated_at`

func scanManagedPool(row interface{ Scan(...any) error }) (ManagedPool, error) {
	var p ManagedPool
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
			return ManagedPool{}, fmt.Errorf("invalid stored managed pool snapshot: %w", err)
		}
	}
	return p, nil
}

// CreateManagedPool activates public provider configuration with revision one.
func (s *Store) CreateManagedPool(ctx context.Context, p ManagedPool) (ManagedPool, error) {
	if err := validateManagedPool(p); err != nil {
		return ManagedPool{}, err
	}
	p.SourceFingerprint = sourceFingerprint(p.Source)
	p.Revision = 1
	p.CreatedAt = nowRFC3339()
	p.UpdatedAt = p.CreatedAt
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO managed_pools (`+managedPoolCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.UID, p.Name, p.Connection, encodeManaged(p.Source), p.SourceFingerprint, encodeManaged(p.Labels), p.MaxActors, p.Resources.CPUMillicores, p.Resources.MemoryBytes, p.Resources.DiskBytes, encodeManaged(p.Template), p.State, p.Revision, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return ManagedPool{}, wrapUnique(err, "managed pool")
	}
	return p, nil
}

// GetManagedPool returns a pool including deleted tombstones.
func (s *Store) GetManagedPool(ctx context.Context, uid string) (ManagedPool, error) {
	p, err := scanManagedPool(s.conn(ctx).QueryRowContext(ctx, `SELECT `+managedPoolCols+` FROM managed_pools WHERE uid=?`, uid))
	if err == sql.ErrNoRows {
		err = fmt.Errorf("%w: managed pool", ErrNotFound)
	}
	return p, err
}

// ListManagedPools returns all pools, including tombstones, in UID order.
func (s *Store) ListManagedPools(ctx context.Context) ([]ManagedPool, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT `+managedPoolCols+` FROM managed_pools ORDER BY uid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pools := []ManagedPool{}
	for rows.Next() {
		p, err := scanManagedPool(rows)
		if err != nil {
			return nil, err
		}
		pools = append(pools, p)
	}
	return pools, rows.Err()
}

// UpdateManagedPool replaces mutable configuration with optimistic concurrency.
// Connection and template bindings cannot change while allocations exist.
// Capacity cannot shrink below current reservations; draining permits stop/delete.
func (s *Store) UpdateManagedPool(ctx context.Context, p ManagedPool, expected int64) (ManagedPool, error) {
	var result ManagedPool
	err := s.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.GetManagedPool(ctx, p.UID)
		if err != nil {
			return err
		}
		if old.Revision != expected {
			return ErrRevisionConflict
		}
		if old.State == "deleted" {
			return ErrPoolUnavailable
		}
		if err := validateManagedPool(p); err != nil {
			return err
		}
		count, used, err := s.managedPoolUsage(ctx, p.UID)
		if err != nil {
			return err
		}
		if count > 0 && (old.Connection != p.Connection || old.Source != p.Source || old.Template != p.Template) {
			return ErrManagedBinding
		}
		if !managedFits(p, count, used, ManagedResources{}) {
			return ErrPoolCapacity
		}
		p.SourceFingerprint = sourceFingerprint(p.Source)
		p.CreatedAt = old.CreatedAt
		p.UpdatedAt = nowRFC3339()
		p.Revision = expected + 1
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_pools SET name=?,connection=?,source=?,source_fingerprint=?,labels=?,max_actors=?,cpu_millicores=?,memory_bytes=?,disk_bytes=?,template=?,state=?,revision=?,updated_at=? WHERE uid=?`, p.Name, p.Connection, encodeManaged(p.Source), p.SourceFingerprint, encodeManaged(p.Labels), p.MaxActors, p.Resources.CPUMillicores, p.Resources.MemoryBytes, p.Resources.DiskBytes, encodeManaged(p.Template), p.State, p.Revision, p.UpdatedAt, p.UID)
		result = p
		return wrapUnique(err, "managed pool")
	})
	return result, err
}

// DeleteManagedPool retains an audit tombstone after checking all live allocations.
func (s *Store) DeleteManagedPool(ctx context.Context, uid string, expected int64) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.GetManagedPool(ctx, uid)
		if err != nil {
			return err
		}
		if p.Revision != expected {
			return ErrRevisionConflict
		}
		count, _, err := s.managedPoolUsage(ctx, uid)
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrManagedChildren
		}
		if p.State == "deleted" {
			return nil
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_pools SET state='deleted',revision=revision+1,updated_at=? WHERE uid=?`, nowRFC3339(), uid)
		return err
	})
}

func (s *Store) managedPoolUsage(ctx context.Context, uid string) (int64, ManagedResources, error) {
	var count int64
	var used ManagedResources
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*),coalesce(sum(cpu_millicores),0),coalesce(sum(memory_bytes),0),coalesce(sum(disk_bytes),0) FROM managed_actors WHERE pool_uid=? AND phase<>'deleted'`, uid).Scan(&count, &used.CPUMillicores, &used.MemoryBytes, &used.DiskBytes)
	return count, used, err
}
func managedFits(p ManagedPool, count int64, used, extra ManagedResources) bool {
	return count <= p.MaxActors && used.CPUMillicores <= p.Resources.CPUMillicores && extra.CPUMillicores <= p.Resources.CPUMillicores-used.CPUMillicores && used.MemoryBytes <= p.Resources.MemoryBytes && extra.MemoryBytes <= p.Resources.MemoryBytes-used.MemoryBytes && used.DiskBytes <= p.Resources.DiskBytes && extra.DiskBytes <= p.Resources.DiskBytes-used.DiskBytes
}

const managedActorCols = `uid,name,swarm_id,atespace_uid,pool_uid,cluster_identity,source_fingerprint,template,incarnation,phase,desired,cpu_millicores,memory_bytes,disk_bytes,ctid,owner_nonce,operation_id,operation,upid,error,revision,created_at,updated_at,deleted_at,stage,role,delegator_id,parent_uid`

func scanManagedActor(row interface{ Scan(...any) error }) (ManagedActor, error) {
	var a ManagedActor
	var template string
	err := row.Scan(&a.UID, &a.Name, &a.SwarmID, &a.AtespaceUID, &a.PoolUID, &a.ClusterIdentity, &a.SourceFingerprint, &template, &a.Incarnation, &a.Phase, &a.Desired, &a.Resources.CPUMillicores, &a.Resources.MemoryBytes, &a.Resources.DiskBytes, &a.CTID, &a.OwnerNonce, &a.OperationID, &a.Operation, &a.UPID, &a.Error, &a.Revision, &a.CreatedAt, &a.UpdatedAt, &a.DeletedAt, &a.Stage, &a.Role, &a.DelegatorID, &a.ParentUID)
	if err == nil {
		err = json.Unmarshal([]byte(template), &a.Template)
	}
	return a, err
}

// ReserveManagedActor atomically reserves live pool capacity across projects.
// Input needs UID, Name, SwarmID, AtespaceUID, PoolUID, Resources, CTID,
// OwnerNonce and a create OperationID. The store snapshots all binding fields.
// The create intent must commit before the caller writes to PVE. When nested in
// WithTx, callers must wait for the outer transaction to commit before any write.
func (s *Store) ReserveManagedActor(ctx context.Context, a ManagedActor) (ManagedActor, error) {
	if a.Role == "" {
		a.Role = "worker"
	}
	if (a.Role != "worker" && a.Role != "master") || (a.Role == "master" && (a.DelegatorID == "" || a.ParentUID != "")) || (a.Role == "worker" && a.DelegatorID != "") {
		return ManagedActor{}, fmt.Errorf("invalid managed actor delegation")
	}
	if a.UID == "" || a.Name == "" || a.SwarmID == "" || a.AtespaceUID == "" || a.PoolUID == "" || a.OwnerNonce == "" || a.OperationID == "" || a.CTID <= 0 || a.Resources.CPUMillicores <= 0 || a.Resources.MemoryBytes <= 0 || a.Resources.DiskBytes <= 0 {
		return ManagedActor{}, fmt.Errorf("managed actor identity, ownership, operation and positive resources are required")
	}
	var result ManagedActor
	err := s.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.GetManagedPool(ctx, a.PoolUID)
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
		if sw.State != "active" || sw.AtespaceUID == nil || *sw.AtespaceUID != a.AtespaceUID {
			return fmt.Errorf("managed actor requires an active matching atespace binding")
		}
		count, used, err := s.managedPoolUsage(ctx, p.UID)
		if err != nil {
			return err
		}
		if count >= p.MaxActors || !managedFits(p, count, used, a.Resources) {
			return ErrPoolCapacity
		}
		a.ClusterIdentity = p.Source.ClusterIdentity
		a.SourceFingerprint = p.SourceFingerprint
		a.Template = p.Template
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
		a.DeletedAt = nil
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO managed_actors (`+managedActorCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.UID, a.Name, a.SwarmID, a.AtespaceUID, a.PoolUID, a.ClusterIdentity, a.SourceFingerprint, encodeManaged(a.Template), a.Incarnation, a.Phase, a.Desired, a.Resources.CPUMillicores, a.Resources.MemoryBytes, a.Resources.DiskBytes, a.CTID, a.OwnerNonce, a.OperationID, a.Operation, a.UPID, a.Error, a.Revision, a.CreatedAt, a.UpdatedAt, a.DeletedAt, a.Stage, a.Role, a.DelegatorID, a.ParentUID)
		result = a
		return wrapUnique(err, "managed actor name or CTID")
	})
	return result, err
}

// GetManagedActor returns immutable identity, intent and latest reconciliation state.
func (s *Store) GetManagedActor(ctx context.Context, uid string) (ManagedActor, error) {
	a, err := scanManagedActor(s.conn(ctx).QueryRowContext(ctx, `SELECT `+managedActorCols+` FROM managed_actors WHERE uid=?`, uid))
	if err == sql.ErrNoRows {
		err = fmt.Errorf("%w: managed actor", ErrNotFound)
	}
	return a, err
}

// ListManagedActors lists all rows for reconciliation; an empty swarmID means all swarms.
// Deleted tombstones are included so callers can distinguish absent from retired UIDs.
func (s *Store) ListManagedActors(ctx context.Context, swarmID string) ([]ManagedActor, error) {
	query := `SELECT ` + managedActorCols + ` FROM managed_actors`
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
	actors := []ManagedActor{}
	for rows.Next() {
		a, err := scanManagedActor(rows)
		if err != nil {
			return nil, err
		}
		actors = append(actors, a)
	}
	return actors, rows.Err()
}

// HasManagedActorsBySwarm includes provisioning, stopped and failed allocations.
// Callers must check this before native swarm deletion under their admission lock.
func (s *Store) HasManagedActorsBySwarm(ctx context.Context, swarmID string) (bool, error) {
	var exists bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_actors WHERE swarm_id=? AND phase<>'deleted')`, swarmID).Scan(&exists)
	return exists, err
}

// UpdateManagedActor CAS-updates only observed Phase, UPID and sanitized Error.
// It cannot release capacity, change ownership, or change operation intent/stage.
// Phase is diagnostic, not task completion. Prefer BindManagedActorTask for UPID;
// this compatibility path can bind only an active, previously unbound task slot.
func (s *Store) UpdateManagedActor(ctx context.Context, a ManagedActor, expected int64) (ManagedActor, error) {
	var result ManagedActor
	err := s.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.GetManagedActor(ctx, a.UID)
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
			return ErrManagedBinding
		}
		if old.Phase == "deleted" || a.Phase == "deleted" {
			return fmt.Errorf("managed deletion requires explicit confirmation")
		}
		if a.Phase != "provisioning" && a.Phase != "running" && a.Phase != "stopped" && a.Phase != "stopping" && a.Phase != "error" && a.Phase != "deleting" {
			return fmt.Errorf("invalid managed actor phase")
		}
		if (old.UPID != "" || old.Stage == "done" || old.Stage == "reconcile") && a.UPID != old.UPID {
			return fmt.Errorf("managed task is already bound or requires stage reconciliation")
		}
		a.Revision = expected + 1
		a.UpdatedAt = nowRFC3339()
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase=?,upid=?,error=?,revision=?,updated_at=? WHERE uid=?`, a.Phase, a.UPID, a.Error, a.Revision, a.UpdatedAt, a.UID)
		result = a
		return err
	})
	return result, err
}

// BeginManagedActorOperation commits intent before any external mutation.
// start/reset require an enabled pool; stop/delete remain legal while draining.
// A matching current ID/kind is idempotent. Unknown outcomes must be reconciled
// before beginning any different operation, including cleanup. Observed Phase alone
// does not reconcile a task: use ConfirmManagedActorStage or the explicit terminal
// failure API first. Stage=done permits a reconciled error to begin fresh work.
// Reset is destructive and retains old ownership until confirmed old absence.
func (s *Store) BeginManagedActorOperation(ctx context.Context, uid string, expected int64, operationID, operation string) (ManagedActor, error) {
	var result ManagedActor
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetManagedActor(ctx, uid)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrRevisionConflict
		}
		if operationID == "" || a.Phase == "deleted" {
			return fmt.Errorf("active managed actor and operation ID are required")
		}
		if a.Desired == "deleted" && operation != "delete" {
			return fmt.Errorf("managed deletion intent cannot be canceled")
		}
		if operationID == a.OperationID {
			if operation != a.Operation {
				return fmt.Errorf("managed operation ID is already bound")
			}
			result = a
			return nil
		}
		if a.Stage != "done" {
			return fmt.Errorf("managed operation task must be reconciled before replacement")
		}
		desired := ""
		switch operation {
		case "start", "reset":
			desired = "running"
			p, err := s.GetManagedPool(ctx, a.PoolUID)
			if err != nil {
				return err
			}
			if p.State != "enabled" {
				return ErrPoolUnavailable
			}
			if a.Phase != "running" && a.Phase != "stopped" && a.Phase != "error" {
				return fmt.Errorf("managed actor must be reconciled before start or reset")
			}
		case "stop":
			if a.Phase != "running" && a.Phase != "stopped" && a.Phase != "error" {
				return fmt.Errorf("managed actor must be reconciled before stop")
			}
			desired = "stopped"
		case "delete":
			desired = "deleted"
		default:
			return fmt.Errorf("invalid managed actor operation")
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
		case "stop", "reset":
			a.Phase = "stopping"
		case "start":
			a.Phase = "provisioning"
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET operation_id=?,operation=?,desired=?,upid='',error='',revision=?,updated_at=?,incarnation=?,phase=?,stage=? WHERE uid=?`, a.OperationID, a.Operation, a.Desired, a.Revision, a.UpdatedAt, a.Incarnation, a.Phase, a.Stage, uid)
		result = a
		return err
	})
	return result, err
}

// ConfirmManagedActorDeleted releases capacity only after caller-confirmed PVE absence.
// Caller must verify owned CT absence AND terminal delete task reconciliation,
// including unknown submissions. This method performs no external calls and
// requires the completed stop stage; absence alone cannot bypass a pending stop.
// OperationID and revision fence stale cleanup observations from newer operations.
func (s *Store) ConfirmManagedActorDeleted(ctx context.Context, uid string, expected int64, operationID string) (ManagedActor, error) {
	var result ManagedActor
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetManagedActor(ctx, uid)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrRevisionConflict
		}
		if a.Operation != "delete" || a.Desired != "deleted" || a.OperationID != operationID {
			return fmt.Errorf("managed deletion intent does not match confirmation")
		}
		if a.Phase == "deleted" {
			result = a
			return nil
		}
		if a.Stage != "delete" {
			return fmt.Errorf("managed stop stage must be confirmed before deletion")
		}
		a.Phase = "deleted"
		a.Stage = "done"
		a.Revision++
		a.UpdatedAt = nowRFC3339()
		a.DeletedAt = ptrManaged(a.UpdatedAt)
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='deleted',stage='done',revision=?,updated_at=?,deleted_at=? WHERE uid=?`, a.Revision, a.UpdatedAt, a.DeletedAt, uid)
		result = a
		return err
	})
	return result, err
}

// BindManagedActorTask records a submitted task for exactly one operation stage.
// Commit the intent before submission, then bind its UPID before polling. A lost
// submission response must be reconciled externally, never blindly resubmitted.
func (s *Store) BindManagedActorTask(ctx context.Context, uid string, expected int64, operationID, stage, upid string) (ManagedActor, error) {
	return s.changeManagedStage(ctx, uid, expected, operationID, stage, func(a *ManagedActor) error {
		if upid == "" || stage == "done" || stage == "reconcile" {
			return fmt.Errorf("active managed task stage and nonempty UPID are required")
		}
		if a.UPID != "" && a.UPID != upid {
			return fmt.Errorf("managed operation UPID is already bound")
		}
		a.UPID = upid
		return nil
	})
}

// ConfirmManagedActorStage records caller-confirmed terminal task reconciliation
// AND owned CT state (running, stopped, or absent). Absence alone is insufficient
// while a task may still create/start the CT. No external calls are made here.
// The caller must reconcile an unknown submission even when UPID is empty.
// Cleanup stop advances to delete with a fresh task slot; normal work becomes
// done without discarding its task evidence. Phase updates alone never do this.
func (s *Store) ConfirmManagedActorStage(ctx context.Context, uid string, expected int64, operationID, stage, observed string) (ManagedActor, error) {
	return s.changeManagedStage(ctx, uid, expected, operationID, stage, func(a *ManagedActor) error {
		if observed != "running" && observed != "stopped" && observed != "absent" {
			return fmt.Errorf("confirmed managed CT state must be running, stopped, or absent")
		}
		if stage == "done" || stage == "delete" {
			return fmt.Errorf("managed stage requires deletion or reset confirmation")
		}
		if stage == "reconcile" && a.Operation == "reset" {
			return fmt.Errorf("legacy reset ownership requires manual reconciliation")
		}
		cleanup := (a.Operation == "delete" || a.Operation == "reset") && (stage == "stop" || stage == "reconcile")
		if (stage == "stop" || (stage == "reconcile" && a.Operation == "stop")) && observed == "running" {
			return fmt.Errorf("managed stop requires confirmed stopped or absent CT")
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
		return nil
	})
}

// ResolveManagedActorTaskFailure records caller-confirmed terminal task failure
// AND an ownership-verified running/stopped CT or globally confirmed absence.
// The caller must verify the exact bound UPID is terminal and failed; observed
// state alone never proves this. Unknown outcomes and unbound submissions must
// not use this API. No external calls are made here.
// Revision, operation ID, stage and failed UPID fence the assertion. The slot
// becomes done with diagnostic Phase=error, without claiming the target state
// was achieved. Task/error evidence, intent, ownership and capacity are retained.
// Retry requires a fresh operation ID; cleanup retries begin again at stop.
func (s *Store) ResolveManagedActorTaskFailure(ctx context.Context, uid string, expected int64, operationID, stage, failedUPID, observed string) (ManagedActor, error) {
	return s.changeManagedStage(ctx, uid, expected, operationID, stage, func(a *ManagedActor) error {
		if stage != "create" && stage != "start" && stage != "stop" && stage != "delete" {
			return fmt.Errorf("active managed task stage is required for failure resolution")
		}
		if failedUPID == "" || a.UPID != failedUPID {
			return fmt.Errorf("failed managed task UPID must match the bound task")
		}
		if observed != "running" && observed != "stopped" && observed != "absent" {
			return fmt.Errorf("confirmed managed CT state must be running, stopped, or absent")
		}
		a.Stage = "done"
		a.Phase = "error"
		return nil
	})
}

// ConfirmManagedActorReset confirms the old owned CT is absent AND its delete
// task is terminal, then atomically creates a fresh incarnation/create intent.
// Reset destroys the workspace; stop/start retain it. UID, CTID, resource and
// binding reservations are retained continuously, including while errors persist.
// The caller supplies a fresh unpredictable owner nonce, different from the old.
// Operation remains reset; Stage=create identifies its new create task slot.
func (s *Store) ConfirmManagedActorReset(ctx context.Context, uid string, expected int64, operationID, freshNonce string) (ManagedActor, error) {
	return s.changeManagedStage(ctx, uid, expected, operationID, "delete", func(a *ManagedActor) error {
		if a.Operation != "reset" || a.Desired != "running" || freshNonce == "" || freshNonce == a.OwnerNonce {
			return fmt.Errorf("managed reset intent and fresh ownership nonce are required")
		}
		a.Incarnation++
		a.OwnerNonce = freshNonce
		a.Stage = "create"
		a.Phase = "provisioning"
		a.UPID = ""
		a.Error = ""
		return nil
	})
}

func (s *Store) changeManagedStage(ctx context.Context, uid string, expected int64, operationID, stage string, change func(*ManagedActor) error) (ManagedActor, error) {
	var result ManagedActor
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetManagedActor(ctx, uid)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrRevisionConflict
		}
		if operationID == "" || a.OperationID != operationID || a.Stage != stage || a.Phase == "deleted" {
			return fmt.Errorf("managed operation stage does not match confirmation")
		}
		if err := change(&a); err != nil {
			return err
		}
		a.Revision++
		a.UpdatedAt = nowRFC3339()
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET stage=?,phase=?,upid=?,error=?,incarnation=?,owner_nonce=?,revision=?,updated_at=? WHERE uid=?`, a.Stage, a.Phase, a.UPID, a.Error, a.Incarnation, a.OwnerNonce, a.Revision, a.UpdatedAt, uid)
		result = a
		return err
	})
	return result, err
}

func ptrManaged(value string) *string { return &value }

// LookupManagedActor resolves an actor by its name within one swarm for
// swarm-scoped delivery. Deleted tombstones resolve to ErrNotFound.
func (s *Store) LookupManagedActor(ctx context.Context, swarmID, name string) (ManagedActor, error) {
	a, err := scanManagedActor(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+managedActorCols+` FROM managed_actors WHERE swarm_id=? AND name=? AND phase<>'deleted'`, swarmID, name))
	if err == sql.ErrNoRows {
		return a, fmt.Errorf("%w: managed actor", ErrNotFound)
	}
	return a, err
}
