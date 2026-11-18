package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// EphemeralActorReservation contains only server-selected physical reservation
// fields. Scope, class, role, parent, pool, origin and name come from the intent.
type EphemeralActorReservation struct {
	UID, OwnerNonce, OperationID string
	CTID                         int
	Resources                    ManagedResources
}

// EphemeralChildAllocation retains one worker identity for an immutable intent.
// It is inventory admission, not a bootstrap, delivery or result receipt.
type EphemeralChildAllocation struct {
	IntentID, ActorUID           string
	ActorIncarnation             int64
	CreateOperationID, CreatedAt string
}

// EphemeralActorAdmission validates full project admission under the caller's
// shared project gate. It must include native and managed reservations, use ctx
// for Store reads, and perform neither network I/O nor mutations. Nil denies new
// allocations. Actor contains the exact derived proposed worker and resources.
type EphemeralActorAdmission func(context.Context, EphemeralChildIntent, ManagedActor) error

// AllocateEphemeralChild commits one class-bound actor, allocation and unissued
// create grant with FULL sync before returning. This is not physical permission:
// ReserveEphemeralChildCreate must separately grant one fresh Create.
// Pool exhaustion leaves the intent queued; the dispatcher treats it as waiting.
// Exact retries reauthorize the parent but do not reserve project capacity again.
// An existing binding is history: reconcile its current lifecycle, never replay
// its create operation merely because this call returned the retained identity.
// This does not register a child execution or start bootstrap/instruction delivery.
func (s *Store) AllocateEphemeralChild(ctx context.Context, intentID string, reservation EphemeralActorReservation, authorize SwarmChildAuthorize, admit EphemeralActorAdmission) (EphemeralChildAllocation, error) {
	var result EphemeralChildAllocation
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		var err error
		result, err = s.allocateEphemeralChild(ctx, intentID, reservation, authorize, admit)
		return err
	})
	if err != nil {
		return EphemeralChildAllocation{}, err
	}
	return result, nil
}

// allocateEphemeralChild runs only inside the caller's FULL transaction.
func (s *Store) allocateEphemeralChild(ctx context.Context, intentID string, reservation EphemeralActorReservation, authorize SwarmChildAuthorize, admit EphemeralActorAdmission) (EphemeralChildAllocation, error) {
	if !validPipelineField(intentID) || !validPipelineField(reservation.UID) || !validPipelineField(reservation.OwnerNonce) || !validPipelineField(reservation.OperationID) || reservation.CTID <= 0 || reservation.Resources.CPUMillicores <= 0 || reservation.Resources.MemoryBytes <= 0 || reservation.Resources.DiskBytes <= 0 {
		return EphemeralChildAllocation{}, ErrSwarmChildConflict
	}
	encoded, err := json.Marshal(reservation)
	if err != nil {
		return EphemeralChildAllocation{}, ErrSwarmChildConflict
	}
	sum := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(sum[:])
	var result EphemeralChildAllocation
	err = s.WithTx(ctx, func(ctx context.Context) error {
		intent, err := s.GetEphemeralChildIntent(ctx, intentID)
		if err != nil {
			return err
		}
		executor := intent.Parent.Principal
		executor.ActorUID, executor.Incarnation = reservation.UID, 1
		if err := s.authorizeChild(ctx, SwarmChildAuthorization{Action: "allocate_ephemeral", Caller: intent.Parent.Principal, Parent: intent.Parent, Executor: executor, RequestID: intent.RequestID, Payload: intent.Payload}, authorize); err != nil {
			return err
		}
		if err := s.checkEphemeralChildNotFailed(ctx, intent.ID); err != nil {
			return err
		}
		old, hash, err := s.ephemeralChildAllocation(ctx, intent.ID)
		if err == nil {
			if hash != fingerprint {
				return ErrSwarmChildConflict
			}
			result = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if admit == nil {
			return ErrSwarmChildDenied
		}
		join, err := s.loadChildJoin(ctx, intent.Parent)
		if err != nil {
			return err
		}
		if join.State != "open" || join.Interrupted {
			return ErrSwarmChildConflict
		}
		for _, task := range join.Children {
			if task.RequestID == intent.RequestID {
				return ErrSwarmChildConflict
			}
		}
		pool, err := s.GetManagedPool(ctx, intent.PoolUID)
		if err != nil {
			return err
		}
		if pool.State != "enabled" {
			return ErrPoolUnavailable
		}
		if pool.SourceFingerprint != intent.SourceFingerprint || pool.Template != intent.Template {
			return ErrManagedBinding
		}
		swarm, err := s.GetSwarm(ctx, intent.Parent.Principal.SwarmID)
		if err != nil {
			return err
		}
		first, err := s.GetSwarmStep(ctx, swarm.ID, intent.Parent.Pass)
		if err != nil {
			return err
		}
		if swarm.Source == nil || swarm.Source.Origin != intent.Origin || first.SourceCommit != intent.SourceCommit {
			return ErrSwarmChildConflict
		}
		name := "e-" + strconv.FormatInt(intent.Parent.Sequence, 36) + "-" + strings.ToLower(intent.ID)
		if len(name) > 63 {
			return ErrSwarmChildConflict
		}
		actor := ManagedActor{Class: ActorClassEphemeral, Role: "worker", ParentUID: intent.Parent.Principal.ActorUID, UID: reservation.UID, Name: name, SwarmID: swarm.ID, AtespaceUID: intent.Parent.Principal.AtespaceUID, PoolUID: intent.PoolUID, Origin: intent.Origin, Resources: reservation.Resources, CTID: reservation.CTID, OwnerNonce: reservation.OwnerNonce, OperationID: reservation.OperationID, Template: intent.Template, SourceFingerprint: intent.SourceFingerprint, ClusterIdentity: pool.Source.ClusterIdentity}
		if err := admit(ctx, intent, actor); err != nil {
			return err
		}
		// Новый intent не может усыновить существующий UID даже при IGNORE.
		if _, err := s.GetManagedActor(ctx, actor.UID); err == nil {
			return ErrSwarmChildConflict
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		stored, err := s.reserveManagedActor(ctx, actor)
		if err != nil {
			return err
		}
		persisted, err := s.GetManagedActor(ctx, stored.UID)
		if err != nil {
			return err
		}
		var network string
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT policy FROM managed_actor_networks WHERE actor_uid=?`, stored.UID).Scan(&network); err != nil {
			return err
		}
		if persisted != stored || network != stored.NetworkPolicy {
			return ErrSwarmChildConflict
		}
		result = EphemeralChildAllocation{IntentID: intent.ID, ActorUID: stored.UID, ActorIncarnation: stored.Incarnation, CreateOperationID: stored.OperationID, CreatedAt: nowRFC3339()}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO ephemeral_child_allocations(intent_id,actor_uid,actor_incarnation,create_operation_id,reservation_hash,created_at) VALUES(?,?,?,?,?,?)`, result.IntentID, result.ActorUID, result.ActorIncarnation, result.CreateOperationID, fingerprint, result.CreatedAt)
		if err != nil {
			return err
		}
		// RAISE(IGNORE) не является ошибкой SQL: проверяем сохранённую запись.
		retained, hash, err := s.ephemeralChildAllocation(ctx, intent.ID)
		if err != nil {
			return err
		}
		if retained != result || hash != fingerprint {
			return ErrSwarmChildConflict
		}
		// Триггер и проверка не допускают частичного allocation без queued grant.
		grant, err := s.GetEphemeralChildCreateGrant(ctx, intent.ID)
		if err != nil {
			return err
		}
		if grant.State != "queued" || grant.ActorUID != result.ActorUID || grant.ActorIncarnation != result.ActorIncarnation || grant.CreateOperationID != result.CreateOperationID || grant.CreatedAt != result.CreatedAt || grant.ReservedAt != "" {
			return ErrSwarmChildConflict
		}
		return nil
	})
	if err != nil {
		return EphemeralChildAllocation{}, err
	}
	return result, nil
}

// ErrEphemeralChildGroupPending means expected slots still lack intents or actual
// members. Like ErrPoolCapacity, it is waiting, not a child failure or cleanup grant.
var ErrEphemeralChildGroupPending = errors.New("ephemeral child group is waiting for all expected slots")

// AllocateEphemeralChildGroup reserves every still-unallocated intent of one exact
// current parent group in one nonnested FULL transaction. All expected slots must
// be accounted for by intents or actual members. Missing intents and capacity
// remain queued; no limit is increased and no failure or early cleanup is recorded.
//
// Reservations are server-selected, keyed by intent ID. Every unallocated intent
// requires one; already allocated intents may be omitted, or included only with
// their exact retained reservation. Unknown IDs are refused. Returned bindings
// correspond to supplied reservations in intent FIFO order. Retained bindings are
// history, never permission to replay or retarget an issued Create.
//
// Pools must contain a known current snapshot for every intent's pool, including
// revision, capacity, source and template. Both callbacks are mandatory, including
// on retries. Authorization checks allocate_ephemeral_group and then
// allocate_ephemeral for each supplied reservation. Admission checks each new
// actor and must count native and managed usage using
// ctx, so it sees earlier reservations in this same transaction. The caller holds
// its shared project admission gate. Neither callback may mutate or do network or
// provider I/O. Physical Create still requires a separate grant and positive
// provider reads; destruction still requires the applied aggregate parent join.
func (s *Store) AllocateEphemeralChildGroup(ctx context.Context, parent SwarmChildParent, reservations map[string]EphemeralActorReservation, pools map[string]ManagedPool, authorize SwarmChildAuthorize, admit EphemeralActorAdmission) ([]EphemeralChildAllocation, error) {
	var result []EphemeralChildAllocation
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		if authorize == nil || admit == nil {
			return ErrSwarmChildDenied
		}
		if err := s.authorizeChild(ctx, SwarmChildAuthorization{Action: "allocate_ephemeral_group", Caller: parent.Principal, Parent: parent}, authorize); err != nil {
			return err
		}
		join, err := s.loadChildJoin(ctx, parent)
		if err != nil {
			return err
		}
		if join.State != "open" || join.Interrupted || len(join.UnregisteredFailures) != 0 || len(join.ExecutorFailures) != 0 {
			return ErrSwarmChildConflict
		}
		// Загружаем всю группу, не глобальную страницу FIFO с limit 128.
		rows, err := s.conn(ctx).QueryContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE swarm_id=? AND parent_sequence=? ORDER BY rowid LIMIT 65`, parent.Principal.SwarmID, parent.Sequence)
		if err != nil {
			return err
		}
		var intents []EphemeralChildIntent
		for rows.Next() {
			intent, err := scanEphemeralChildIntent(rows)
			if err != nil {
				rows.Close()
				return err
			}
			intents = append(intents, intent)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		members := make(map[string]SwarmChildTask, len(join.Children))
		for _, member := range join.Children {
			members[member.RequestID] = member
		}
		slots, supplied := len(members), 0
		for _, intent := range intents {
			if intent.Parent != parent {
				return ErrSwarmChildConflict
			}
			if err := s.checkEphemeralChildNotFailed(ctx, intent.ID); err != nil {
				return err
			}
			pool, err := s.GetManagedPool(ctx, intent.PoolUID)
			if err != nil {
				return err
			}
			snapshot, known := pools[intent.PoolUID]
			if !known || snapshot.UID != pool.UID || snapshot.Revision != pool.Revision || snapshot.Resources != pool.Resources || snapshot.MaxActors != pool.MaxActors || snapshot.State != pool.State || snapshot.SourceFingerprint != pool.SourceFingerprint || snapshot.Template != pool.Template || snapshot.Source != pool.Source || snapshot.Connection != pool.Connection || pool.SourceFingerprint != intent.SourceFingerprint || pool.Template != intent.Template {
				return ErrManagedBinding
			}
			if pool.State != "enabled" {
				return ErrPoolUnavailable
			}
			allocation, _, err := s.ephemeralChildAllocation(ctx, intent.ID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			_, provided := reservations[intent.ID]
			if provided {
				supplied++
			} else if errors.Is(err, ErrNotFound) {
				return ErrSwarmChildConflict
			}
			if member, ok := members[intent.RequestID]; ok {
				if err != nil || member.Payload != intent.Payload || member.Executor.ActorUID != allocation.ActorUID || member.Executor.Incarnation != allocation.ActorIncarnation {
					return ErrSwarmChildConflict
				}
			} else {
				slots++
			}
		}
		if supplied != len(reservations) || slots > join.Policy.Total {
			return ErrSwarmChildConflict
		}
		if slots < join.Policy.Total {
			return ErrEphemeralChildGroupPending
		}
		for _, intent := range intents {
			reservation, provided := reservations[intent.ID]
			if !provided {
				continue
			}
			allocation, err := s.allocateEphemeralChild(ctx, intent.ID, reservation, authorize, admit)
			if err != nil {
				return err
			}
			result = append(result, allocation)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ephemeralChildAllocation(ctx context.Context, intentID string) (EphemeralChildAllocation, string, error) {
	var result EphemeralChildAllocation
	var hash string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT intent_id,actor_uid,actor_incarnation,create_operation_id,created_at,reservation_hash FROM ephemeral_child_allocations WHERE intent_id=?`, intentID).Scan(&result.IntentID, &result.ActorUID, &result.ActorIncarnation, &result.CreateOperationID, &result.CreatedAt, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return EphemeralChildAllocation{}, "", ErrNotFound
	}
	if err != nil {
		return EphemeralChildAllocation{}, "", err
	}
	return result, hash, nil
}

// GetEphemeralChildAllocation reads retained internal identity, not permission
// to provision or deliver. Adapters enforce current visibility or caller scope.
func (s *Store) GetEphemeralChildAllocation(ctx context.Context, intentID string) (EphemeralChildAllocation, error) {
	result, _, err := s.ephemeralChildAllocation(ctx, intentID)
	return result, err
}
