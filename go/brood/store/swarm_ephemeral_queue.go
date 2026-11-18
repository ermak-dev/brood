package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EphemeralChildIntent retains work before actor capacity is allocated. It is
// not a child execution, instruction receipt, actor lease or permission to create
// a worker. QueuePosition is the stable SQLite insertion cursor, not a timestamp.
type EphemeralChildIntent struct {
	ID, RequestID, Payload, PoolUID string
	Parent                          SwarmChildParent
	SourceFingerprint               string
	Template                        ManagedTemplateSnapshot
	Origin, SourceCommit, CreatedAt string
	QueuePosition                   int64
}

const ephemeralChildIntentCols = `id,swarm_id,parent_sequence,pass,project_id,atespace_uid,parent_uid,parent_incarnation,parent_request_id,request_id,payload,pool_uid,source_fingerprint,template,source_commit,origin,created_at`

// EnqueueEphemeralChild records a bounded child slot without reserving an actor
// or testing current pool capacity. The pool must be enabled and its approved
// template is snapshotted with the exact parent-pass source. Exact retries
// reauthorize and return the original immutable intent. A later trusted allocator
// must separately admit capacity and bind the resulting worker before any RPC.
func (s *Store) EnqueueEphemeralChild(ctx context.Context, p SwarmChildParent, policy SwarmChildJoinPolicy, key, payload, poolUID string, authorize SwarmChildAuthorize) (EphemeralChildIntent, error) {
	if !validChildRequestID(key) || strings.TrimSpace(payload) == "" || plainTextProblem(payload, MaxSwarmGoalBytes) != "" || !validPipelineField(poolUID) {
		return EphemeralChildIntent{}, ErrSwarmChildConflict
	}
	var result EphemeralChildIntent
	err := s.WithTx(ctx, func(ctx context.Context) (err error) {
		if _, err = s.conn(ctx).ExecContext(ctx, `SAVEPOINT ephemeral_child_enqueue`); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err != nil {
				_, rollback := s.conn(cleanup).ExecContext(cleanup, `ROLLBACK TO ephemeral_child_enqueue`)
				err = errors.Join(err, rollback)
			}
			_, release := s.conn(cleanup).ExecContext(cleanup, `RELEASE ephemeral_child_enqueue`)
			err = errors.Join(err, release)
		}()
		if err = s.authorizeChild(ctx, SwarmChildAuthorization{Action: "enqueue_ephemeral", Caller: p.Principal, Parent: p, RequestID: key, Payload: payload}, authorize); err != nil {
			return err
		}
		join, err := s.CreateSwarmChildJoin(ctx, p, policy, authorize)
		if err != nil {
			return err
		}
		old, err := s.ephemeralChildIntentByRequest(ctx, p.Principal.SwarmID, p.Sequence, key)
		if err == nil {
			if old.Parent != p || old.Payload != payload || old.PoolUID != poolUID {
				return ErrSwarmChildConflict
			}
			result = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if join.State != "open" || join.Interrupted {
			return ErrSwarmChildConflict
		}
		for _, child := range join.Children {
			if child.RequestID == key {
				return ErrSwarmChildConflict
			}
		}
		queued, err := s.unregisteredEphemeralChildCount(ctx, p)
		if err != nil {
			return err
		}
		if len(join.Children)+queued >= join.Policy.Total {
			return ErrSwarmChildConflict
		}
		pool, err := s.GetManagedPool(ctx, poolUID)
		if err != nil {
			return err
		}
		if pool.State != "enabled" {
			return ErrPoolUnavailable
		}
		swarm, err := s.GetSwarm(ctx, p.Principal.SwarmID)
		if err != nil {
			return err
		}
		first, err := s.GetSwarmStep(ctx, swarm.ID, p.Pass)
		if err != nil {
			return err
		}
		if swarm.Source == nil || first.Goal == "" || first.Pass != p.Pass || first.SourceCommit == "" {
			return ErrSwarmChildConflict
		}
		id := NewID()
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO ephemeral_child_intents (`+ephemeralChildIntentCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, swarm.ID, p.Sequence, p.Pass, p.Principal.ProjectID, p.Principal.AtespaceUID, p.Principal.ActorUID, p.Principal.Incarnation, p.RequestID, key, payload, poolUID, pool.SourceFingerprint, encodeManaged(pool.Template), first.SourceCommit, swarm.Source.Origin, nowRFC3339())
		if err != nil {
			return ephemeralSlotError(err)
		}
		result, err = s.GetEphemeralChildIntent(ctx, id)
		return err
	})
	if err != nil {
		return EphemeralChildIntent{}, err
	}
	return result, nil
}

func ephemeralSlotError(err error) error {
	if err != nil && strings.Contains(err.Error(), "ephemeral child slot conflicts") {
		return ErrSwarmChildConflict
	}
	return err
}

func scanEphemeralChildIntent(row interface{ Scan(...any) error }) (EphemeralChildIntent, error) {
	var result EphemeralChildIntent
	var template string
	err := row.Scan(&result.QueuePosition, &result.ID, &result.Parent.Principal.SwarmID, &result.Parent.Sequence, &result.Parent.Pass, &result.Parent.Principal.ProjectID, &result.Parent.Principal.AtespaceUID, &result.Parent.Principal.ActorUID, &result.Parent.Principal.Incarnation, &result.Parent.RequestID, &result.RequestID, &result.Payload, &result.PoolUID, &result.SourceFingerprint, &template, &result.SourceCommit, &result.Origin, &result.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EphemeralChildIntent{}, ErrNotFound
	}
	if err != nil {
		return EphemeralChildIntent{}, err
	}
	if json.Unmarshal([]byte(template), &result.Template) != nil {
		return EphemeralChildIntent{}, fmt.Errorf("invalid retained ephemeral template")
	}
	return result, nil
}

func (s *Store) ephemeralChildIntentByRequest(ctx context.Context, swarm string, sequence int64, key string) (EphemeralChildIntent, error) {
	return scanEphemeralChildIntent(s.conn(ctx).QueryRowContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE swarm_id=? AND parent_sequence=? AND request_id=?`, swarm, sequence, key))
}

// GetEphemeralChildIntent reads internal immutable history. Adapters must check
// the current human visibility or exact authenticated parent scope before use.
// Reading does not allocate, deliver, authorize or change join membership.
func (s *Store) GetEphemeralChildIntent(ctx context.Context, id string) (EphemeralChildIntent, error) {
	return scanEphemeralChildIntent(s.conn(ctx).QueryRowContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE id=?`, id))
}

// QueuedEphemeralChildIntents reads at most 128 FIFO intents after a stable
// insertion cursor. This internal page is not dispatch authority; the allocator
// must reauthorize parent, pool, source and capacity in its write transaction.
func (s *Store) QueuedEphemeralChildIntents(ctx context.Context, after int64) ([]EphemeralChildIntent, error) {
	if after < 0 {
		return nil, ErrSwarmChildConflict
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE rowid>? AND NOT EXISTS(SELECT 1 FROM ephemeral_child_allocations a WHERE a.intent_id=ephemeral_child_intents.id) AND NOT EXISTS(SELECT 1 FROM ephemeral_child_failures f WHERE f.intent_id=ephemeral_child_intents.id) ORDER BY rowid LIMIT 128`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []EphemeralChildIntent{}
	for rows.Next() {
		intent, err := scanEphemeralChildIntent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
