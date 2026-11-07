package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
)

//go:embed coordination.sql
var coordinationSchema string

// ErrCoordinationConflict identifies an occupied slot, stale identity, or mismatched request.
var ErrCoordinationConflict = errors.New("coordination conflict")

// CoordinationConflictError is a stable domain conflict, not a SQLite constraint error.
type CoordinationConflictError struct{ Reason string }

func (e *CoordinationConflictError) Error() string { return "coordination conflict: " + e.Reason }
func (e *CoordinationConflictError) Unwrap() error { return ErrCoordinationConflict }
func newCoordinationConflict(reason string) error  { return &CoordinationConflictError{Reason: reason} }

// CoordinationActor binds to managed actor UID and Incarnation, never a display name.
type CoordinationActor struct {
	UID        string
	Generation int64
}

// CoordinationClaim is both the acquisition request and the exact release capability.
// RequestID is unique within project and primitive kind and must never be reused for
// a different request. HumanID records the human grant, not an authorization decision.
// Source owns the claim; dispatch additionally binds Target in the same swarm.
// Landing requires an empty Target. All current-admin/delegator authorization belongs
// to the server and must be rechecked before invoking these methods, including retries.
type CoordinationClaim struct {
	ProjectID   string
	SwarmID     string
	RequestID   string
	Source      CoordinationActor
	Target      CoordinationActor
	HumanID     string
	PayloadHash string
}

// ClaimDispatchLease atomically claims a target. Claims do not expire. Exact retries
// return the original claim, even after release, without resurrecting ownership.
func (s *Store) ClaimDispatchLease(ctx context.Context, c CoordinationClaim) (CoordinationClaim, error) {
	return s.takeCoordination(ctx, "dispatch", c)
}

// ReleaseDispatchLease releases only the exact original claim. Repeated releases
// succeed and cannot release a successor's lease. Revoked actors may still release.
func (s *Store) ReleaseDispatchLease(ctx context.Context, c CoordinationClaim) error {
	return s.giveCoordination(ctx, "dispatch", c)
}

// TakeLandingToken atomically claims the single project landing slot. There is no
// automatic expiry or force stealing: stale owners require an explicit server
// recovery decision followed by release of the exact stored claim, then a fresh take.
func (s *Store) TakeLandingToken(ctx context.Context, c CoordinationClaim) (CoordinationClaim, error) {
	return s.takeCoordination(ctx, "landing", c)
}

// ShowDispatchLease returns the active claim for a scoped target UID, including a
// stale owner, or ErrNotFound. It does not grant authorization to inspect the swarm.
func (s *Store) ShowDispatchLease(ctx context.Context, projectID, swarmID, targetUID string) (CoordinationClaim, error) {
	c, err := s.showCoordination(ctx, projectID, "dispatch", targetUID)
	if err == nil && c.SwarmID != swarmID {
		return CoordinationClaim{}, ErrNotFound
	}
	return c, err
}

// ShowLandingToken returns the durable owner, including a stale/revoked owner, or ErrNotFound.
func (s *Store) ShowLandingToken(ctx context.Context, projectID string) (CoordinationClaim, error) {
	return s.showCoordination(ctx, projectID, "landing", "")
}

func (s *Store) showCoordination(ctx context.Context, projectID, kind, slot string) (CoordinationClaim, error) {
	var c CoordinationClaim
	var payload string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT payload FROM coordination_claims WHERE project_id=? AND kind=? AND slot=?`, projectID, kind, slot).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(payload), &c)
	return c, err
}

// GiveLandingToken releases only the exact stored token. It grants no permission
// to a caller: the server must authorize the human and source (or explicit recovery).
func (s *Store) GiveLandingToken(ctx context.Context, c CoordinationClaim) error {
	return s.giveCoordination(ctx, "landing", c)
}

func (s *Store) takeCoordination(ctx context.Context, kind string, c CoordinationClaim) (CoordinationClaim, error) {
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := validateCoordinationShape(kind, c); err != nil {
			return err
		}
		payload, found, err := s.coordinationRequest(ctx, kind, c)
		if err != nil {
			return err
		}
		if found {
			if payload != encodeManaged(c) {
				return newCoordinationConflict("request ID already binds a different claim")
			}
			return nil
		}
		if err := s.validateCoordinationOwner(ctx, c); err != nil {
			return err
		}
		slot := ""
		if kind == "dispatch" {
			if err := s.validateCoordinationActor(ctx, c, c.Target); err != nil {
				return err
			}
			slot = c.Target.UID
		}
		var occupied bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_claims WHERE project_id=? AND kind=? AND slot=?)`, c.ProjectID, kind, slot).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return newCoordinationConflict("slot is already owned")
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,?,?,?,?)`, c.ProjectID, kind, c.RequestID, encodeManaged(c), slot)
		return err
	})
	if err != nil {
		return CoordinationClaim{}, err
	}
	return c, nil
}

func validateCoordinationShape(kind string, c CoordinationClaim) error {
	if c.ProjectID == "" || c.SwarmID == "" || c.RequestID == "" || c.HumanID == "" || c.Source.UID == "" || c.Source.Generation <= 0 {
		return newCoordinationConflict("project, swarm, request, human and source identity are required")
	}
	if kind == "dispatch" && (c.Target.UID == "" || c.Target.Generation <= 0) {
		return newCoordinationConflict("target identity is required")
	}
	if kind == "landing" && c.Target != (CoordinationActor{}) {
		return newCoordinationConflict("landing has no target")
	}
	return nil
}

func (s *Store) validateCoordinationOwner(ctx context.Context, c CoordinationClaim) error {
	var valid bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarms s JOIN users u ON u.id=? WHERE s.id=? AND s.project_id=? AND s.state='active' AND u.disabled=0)`, c.HumanID, c.SwarmID, c.ProjectID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return newCoordinationConflict("swarm or human grant identity is unavailable")
	}
	return s.validateCoordinationActor(ctx, c, c.Source)
}

func (s *Store) validateCoordinationActor(ctx context.Context, c CoordinationClaim, a CoordinationActor) error {
	var valid bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_actors a JOIN swarms s ON s.id=a.swarm_id WHERE a.uid=? AND a.incarnation=? AND a.swarm_id=? AND s.project_id=? AND a.atespace_uid=s.atespace_uid AND a.phase NOT IN ('deleting','deleted') AND a.desired='running' AND NOT EXISTS(SELECT 1 FROM coordination_claims c WHERE c.kind='workspace' AND c.slot=a.uid))`, a.UID, a.Generation, c.SwarmID, c.ProjectID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return newCoordinationConflict("managed actor identity is stale or outside scope")
	}
	return nil
}

func (s *Store) coordinationRequest(ctx context.Context, kind string, c CoordinationClaim) (string, bool, error) {
	var payload string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT payload FROM coordination_claims WHERE project_id=? AND kind=? AND request_id=?`, c.ProjectID, kind, c.RequestID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return payload, err == nil, err
}

func (s *Store) giveCoordination(ctx context.Context, kind string, c CoordinationClaim) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if err := validateCoordinationShape(kind, c); err != nil {
			return err
		}
		payload, found, err := s.coordinationRequest(ctx, kind, c)
		if err != nil {
			return err
		}
		if !found || payload != encodeManaged(c) {
			return newCoordinationConflict("release does not match the original claim")
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims SET slot=NULL WHERE project_id=? AND kind=? AND request_id=?`, c.ProjectID, kind, c.RequestID)
		return err
	})
}

func initializeCoordination(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 7 {
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name GLOB 'coordination_*'`).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return fmt.Errorf("incompatible database schema: unversioned coordination objects")
		}
		if _, err := tx.ExecContext(ctx, coordinationSchema+`PRAGMA user_version=8`); err != nil {
			return fmt.Errorf("extend coordination schema: %w", err)
		}
	} else if version != 8 && version != 9 && version != 10 && version != 11 && version != 12 && version != 13 {
		return fmt.Errorf("incompatible database schema: unsupported version %d", version)
	}
	return checkSchema(ctx, tx, coordinationSchema)
}
