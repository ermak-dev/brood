package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ActorCoordinationClaims returns active claims involving an actor's durable UID.
// Stale generations remain held until an explicit, exactly matched release.
func (s *Store) ActorCoordinationClaims(ctx context.Context, uid string) ([]CoordinationClaim, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT payload FROM coordination_claims WHERE slot IS NOT NULL AND (json_extract(payload,'$.Source.UID')=? OR json_extract(payload,'$.Target.UID')=?) LIMIT 1001`, uid, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claims := []CoordinationClaim{}
	for rows.Next() {
		var data string
		var claim CoordinationClaim
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &claim); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
		if len(claims) > 1000 {
			return nil, fmt.Errorf("actor coordination inventory exceeds limit")
		}
	}
	return claims, rows.Err()
}

func (s *Store) guardActorCoordination(ctx context.Context, uid string) error {
	if _, err := s.CurrentActorOperation(ctx, uid); err == nil {
		return newCoordinationConflict("workspace operation is active or unconfirmed")
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	claims, err := s.ActorCoordinationClaims(ctx, uid)
	if err != nil {
		return err
	}
	if len(claims) > 0 {
		return newCoordinationConflict("release active dispatch leases and landing tokens before changing actor lifecycle")
	}
	return nil
}
