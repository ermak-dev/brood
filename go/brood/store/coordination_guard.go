package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

// DescribeCoordinationHold describes active claim c for a refusal read inside
// swarmID: what it holds, its request ID, its holder and how to release it.
// An actor is named only while it is the claimed live incarnation in swarmID;
// otherwise it is unknown. A claim of another swarm is not described.
func (s *Store) DescribeCoordinationHold(ctx context.Context, swarmID string, c CoordinationClaim) (string, error) {
	// Отказ читают актор или человек роя swarmID: в тексте только имена его
	// акторов, request ID и команда освобождения, без человека и хеша задачи.
	if c.SwarmID != swarmID {
		return "an active coordination claim outside this swarm", nil
	}
	holder, err := s.swarmActorName(ctx, swarmID, c.Source)
	if err != nil {
		return "", err
	}
	who := "the holder or an administrator"
	if holder == unknownActor {
		// Незнакомый держатель свою аренду не отпустит: остаётся точный
		// release администратора.
		who = "an administrator"
	}
	if c.Target == (CoordinationActor{}) {
		return fmt.Sprintf("landing token %q is held by %s; %s gives it back with landing --action give --request-id %q", c.RequestID, holder, who, c.RequestID), nil
	}
	target, err := s.swarmActorName(ctx, swarmID, c.Target)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("dispatch lease %q on %s is held by %s; %s releases it with dispatch --action release --request-id %q", c.RequestID, target, holder, who, c.RequestID), nil
}

// unknownActor stands for an actor that is not the claimed live incarnation.
const unknownActor = "an unknown actor"

// swarmActorName quotes the name of a while it is that live incarnation in swarmID.
func (s *Store) swarmActorName(ctx context.Context, swarmID string, a CoordinationActor) (string, error) {
	// Claim связан с UID и поколением, а не с именем: имя другого поколения,
	// роя или удалённого актора (его имя может занять новый) было бы догадкой.
	actor, err := s.GetManagedActor(ctx, a.UID)
	if errors.Is(err, ErrNotFound) {
		return unknownActor, nil
	}
	if err != nil {
		return "", err
	}
	if actor.SwarmID != swarmID || actor.Incarnation != a.Generation || actor.Phase == "deleted" {
		return unknownActor, nil
	}
	return strconv.Quote(actor.Name), nil
}

func (s *Store) guardActorCoordination(ctx context.Context, a ManagedActor) error {
	return s.guardActorCoordinationForIntent(ctx, a, "")
}

func (s *Store) guardActorCoordinationForIntent(ctx context.Context, a ManagedActor, intent string) error {
	if err := s.guardChildLifecycle(ctx, a.UID); err != nil {
		return err
	}
	if op, err := s.CurrentActorOperation(ctx, a.UID); err == nil {
		// Явный stop отзывает живой browser stream, но не освобождает slot.
		// Desired=stopped в той же транзакции запрещает новые child claims.
		// Неизвестный эффект и hold прежней инкарнации не обходятся этим путём.
		if intent != "stop" || op.Operation != "terminal_stream" || op.State != "inflight" || op.Generation != a.Incarnation {
			return newCoordinationConflict("workspace operation is active or unconfirmed")
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	claims, err := s.ActorCoordinationClaims(ctx, a.UID)
	if err != nil {
		return err
	}
	if len(claims) == 0 {
		return nil
	}
	// Отказ называет держателя и request ID: без них не понять, кого просить
	// и что освобождать (#272). Остальные claims — числом, их назовёт повтор.
	hold, err := s.DescribeCoordinationHold(ctx, a.SwarmID, claims[0])
	if err != nil {
		return err
	}
	if len(claims) > 1 {
		hold += fmt.Sprintf(" (and %d more)", len(claims)-1)
	}
	return newCoordinationConflict("release active dispatch leases and landing tokens before changing actor lifecycle: " + hold)
}
