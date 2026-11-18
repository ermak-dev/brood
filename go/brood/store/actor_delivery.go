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
	"time"
	"unicode/utf8"
)

// MaxActorDeliveryAttempts bounds all sends and read-only probes for one message.
const MaxActorDeliveryAttempts = 8

var (
	ErrActorDeliveryConflict = errors.New("delivery source or retained evidence changed")
	ErrActorDeliveryDenied   = errors.New("delivery authority is no longer current")
	ErrActorDeliveryPending  = errors.New("delivery attempt deadline or backoff has not elapsed")
	ErrActorDeliveryBudget   = errors.New("delivery attempt budget exhausted; no further automatic attempts are allowed")
	ErrActorDeliveryComplete = errors.New("delivery already has a retained confirmation")
)

// ActorDeliveryInput pins one immutable logical message, not a transport attempt.
// Payload is the canonical legacy request without server delivery metadata.
// SourceID is the request ID for answers, decimal visit sequence for steps, or
// registered task ID for children. Child preparation uses PrepareEphemeralChildDelivery.
// Callers verify external namespace authority before entering Store; authorize
// rechecks current local authority using its transaction context.
type ActorDeliveryInput struct {
	SwarmID, SourceKind, SourceID   string
	RequestID, Commit               string
	ActorUID                        string
	Incarnation                     int64
	AtespaceUID, JournalID, Payload string
}

// ActorDeliveryIntent retains the original journal even after a guest reconnect.
type ActorDeliveryIntent struct {
	ID        string
	Input     ActorDeliveryInput
	ProbeOnly bool
	CreatedAt string
}

// ActorDeliveryAttempt is permission for send or read-only probe after durable commit.
// Callers must preserve the complete ticket when recording a transport outcome.
type ActorDeliveryAttempt struct {
	IntentID              string
	Number                int
	Mode                  string
	CreatedAt, DeadlineAt time.Time
}

// ActorDeliveryObservation retains an attempt and its optional immutable receipt.
// Empty Outcome means the effect may have started without a recorded response.
type ActorDeliveryObservation struct {
	Attempt         ActorDeliveryAttempt
	Outcome, Detail string
	ObservedAt      time.Time
}

func validActorDeliveryInput(in ActorDeliveryInput) bool {
	if !validPipelineField(in.SwarmID) || !validPipelineField(in.SourceID) || len(in.SourceID) > 512 || !validPipelineField(in.ActorUID) || in.Incarnation < 1 || !validPipelineField(in.AtespaceUID) || len(in.JournalID) != 64 {
		return false
	}
	if in.SourceKind != "step" && in.SourceKind != "answer" && in.SourceKind != "child" {
		return false
	}
	if (in.SourceKind == "step" || in.SourceKind == "child") && (!validPipelineField(in.RequestID) || !validPipelineField(in.Commit)) || in.SourceKind == "answer" && (in.RequestID != "" || in.Commit != "") {
		return false
	}
	if _, err := hex.DecodeString(in.JournalID); err != nil || strings.ToLower(in.JournalID) != in.JournalID {
		return false
	}
	payload := strings.TrimSpace(in.Payload)
	return len(in.Payload) <= 128*1024 && utf8.ValidString(in.Payload) && len(payload) > 0 && payload[0] == '{' && json.Valid([]byte(payload))
}

func (s *Store) actorDeliveryCurrent(ctx context.Context, in ActorDeliveryInput) (bool, error) {
	return s.actorDeliverySource(ctx, in, false)
}

func (s *Store) actorDeliverySource(ctx context.Context, in ActorDeliveryInput, includeConfirmed bool) (bool, error) {
	if in.SourceKind == "child" {
		return s.ephemeralDeliveryCurrent(ctx, in)
	}
	swarm, err := s.GetSwarm(ctx, in.SwarmID)
	if err != nil {
		return false, err
	}
	actor, err := s.GetManagedActor(ctx, in.ActorUID)
	if err != nil {
		return false, err
	}
	if actor.Incarnation != in.Incarnation || swarm.AtespaceUID == nil || *swarm.AtespaceUID != in.AtespaceUID {
		return false, ErrActorDeliveryConflict
	}
	if actor.Class == ActorClassEphemeral {
		if in.SourceKind != "answer" {
			return false, ErrActorDeliveryConflict
		}
		// Здесь проверяется только source; текущую policy отдельно проверяет
		// обязательный callback transport reserve и самого channel-open.
		if err := s.CheckEphemeralChildAnswer(ctx, in.SwarmID, in.SourceID, func(context.Context, SwarmChildAuthorization) error { return nil }); err != nil {
			return false, err
		}
	} else if liveRoleActor(swarm, actor) != nil {
		return false, ErrActorDeliveryConflict
	}
	if in.SourceKind == "answer" {
		r, err := s.GetSwarmRequest(ctx, in.SwarmID, in.SourceID)
		if err != nil {
			return false, err
		}
		deliveryAllowed := r.Delivery == "pending" || r.Delivery == "failed" || r.Delivery == "unconfirmed" || includeConfirmed && r.Delivery == "delivered"
		if r.Origin != "actor" || r.State != "answered" || r.ActorUID != in.ActorUID || r.ActorIncarnation != in.Incarnation || !deliveryAllowed {
			return false, ErrActorDeliveryConflict
		}
		return r.Delivery == "unconfirmed", nil
	}
	sequence, err := strconv.ParseInt(in.SourceID, 10, 64)
	if err != nil || sequence < 1 || strconv.FormatInt(sequence, 10) != in.SourceID || swarm.Execution != "running" && swarm.Execution != "waiting" {
		return false, ErrActorDeliveryConflict
	}
	var state, node, request string
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT state,node_id,request_id FROM swarm_steps WHERE swarm_id=? AND sequence=? AND actor_uid=? AND actor_incarnation=? AND request_id=? AND attested_commit=? AND state IN ('delivering','unconfirmed','inflight') AND (state<>'inflight' OR ?) AND sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?) AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=? AND sequence=?)`, in.SwarmID, sequence, in.ActorUID, in.Incarnation, in.RequestID, in.Commit, includeConfirmed, in.SwarmID, in.SwarmID, sequence).Scan(&state, &node, &request)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrActorDeliveryConflict
	}
	if err != nil {
		return false, err
	}
	_, err = s.currentAttempt(ctx, ActorStepAttempt{SwarmID: in.SwarmID, Sequence: sequence, NodeID: node, RequestID: request, Actor: actor}, state)
	return state == "unconfirmed", err
}

const actorDeliveryColumns = `id,swarm_id,source_kind,source_id,request_id,attested_commit,actor_uid,incarnation,atespace_uid,journal_id,payload,probe_only,created_at`

func scanActorDelivery(row interface{ Scan(...any) error }) (ActorDeliveryIntent, error) {
	var out ActorDeliveryIntent
	err := row.Scan(&out.ID, &out.Input.SwarmID, &out.Input.SourceKind, &out.Input.SourceID, &out.Input.RequestID, &out.Input.Commit, &out.Input.ActorUID, &out.Input.Incarnation, &out.Input.AtespaceUID, &out.Input.JournalID, &out.Input.Payload, &out.ProbeOnly, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// GetActorDelivery reads retained history; the caller enforces swarm visibility.
func (s *Store) GetActorDelivery(ctx context.Context, id string) (ActorDeliveryIntent, error) {
	return scanActorDelivery(s.conn(ctx).QueryRowContext(ctx, `SELECT `+actorDeliveryColumns+` FROM actor_deliveries WHERE id=?`, id))
}

// GetActorDeliveryForSource reads the original scope and payload for a source.
// Callers enforce swarm visibility; a changed peer must not replace this intent.
func (s *Store) GetActorDeliveryForSource(ctx context.Context, swarmID, kind, sourceID string) (ActorDeliveryIntent, error) {
	return scanActorDelivery(s.conn(ctx).QueryRowContext(ctx, `SELECT `+actorDeliveryColumns+` FROM actor_deliveries WHERE swarm_id=? AND source_kind=? AND source_id=?`, swarmID, kind, sourceID))
}

// CheckActorDeliveryCurrent rechecks local source authority at the transport fence.
// External provider and namespace proofs remain the caller's responsibility.
func (s *Store) CheckActorDeliveryCurrent(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		intent, err := s.GetActorDelivery(ctx, id)
		if err != nil {
			return err
		}
		_, err = s.actorDeliveryCurrent(ctx, intent.Input)
		return err
	})
}

// PrepareActorDelivery pins source, payload and journal before a reservation.
// A source already unconfirmed is probe-only even if no old intent survived.
// Ephemeral answers require exact retained question context and a running,
// proof-bound child, not a workflow role. Source validation never replaces the
// mandatory callback's current human and child-scheduling authorization.
func (s *Store) PrepareActorDelivery(ctx context.Context, in ActorDeliveryInput, authorize func(context.Context) bool) (ActorDeliveryIntent, error) {
	var out ActorDeliveryIntent
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.prepareActorDelivery(ctx, in, authorize)
		return err
	})
	if err != nil {
		return ActorDeliveryIntent{}, err
	}
	return out, nil
}

func (s *Store) prepareActorDelivery(ctx context.Context, in ActorDeliveryInput, authorize func(context.Context) bool) (ActorDeliveryIntent, error) {
	if !validActorDeliveryInput(in) || in.SourceKind == "child" {
		return ActorDeliveryIntent{}, ErrActorDeliveryConflict
	}
	if authorize == nil || !authorize(ctx) {
		return ActorDeliveryIntent{}, ErrActorDeliveryDenied
	}
	probe, err := s.actorDeliveryCurrent(ctx, in)
	if err != nil {
		return ActorDeliveryIntent{}, err
	}
	existing, err := s.GetActorDeliveryForSource(ctx, in.SwarmID, in.SourceKind, in.SourceID)
	if err == nil {
		if existing.Input != in {
			return ActorDeliveryIntent{}, ErrActorDeliveryConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return ActorDeliveryIntent{}, err
	}
	return s.insertActorDelivery(ctx, in, probe)
}

func (s *Store) insertActorDelivery(ctx context.Context, in ActorDeliveryInput, probe bool) (ActorDeliveryIntent, error) {
	key, _ := json.Marshal([]string{in.SwarmID, in.SourceKind, in.SourceID})
	hash := sha256.Sum256(key)
	id := "delivery:" + hex.EncodeToString(hash[:])
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_deliveries(`+actorDeliveryColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, in.SwarmID, in.SourceKind, in.SourceID, in.RequestID, in.Commit, in.ActorUID, in.Incarnation, in.AtespaceUID, in.JournalID, in.Payload, probe, nowRFC3339())
	if err != nil {
		return ActorDeliveryIntent{}, err
	}
	return s.GetActorDelivery(ctx, id)
}

// GetActorDeliveryHistory reads bounded immutable transport history.
// The caller enforces the source swarm's current visibility.
func (s *Store) GetActorDeliveryHistory(ctx context.Context, id string) ([]ActorDeliveryObservation, error) {
	if _, err := s.GetActorDelivery(ctx, id); err != nil {
		return nil, err
	}
	return s.actorDeliveryHistory(ctx, id)
}

func (s *Store) actorDeliveryHistory(ctx context.Context, id string) ([]ActorDeliveryObservation, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT a.number,a.mode,a.created_at,a.deadline_at,COALESCE(o.outcome,''),COALESCE(o.detail,''),COALESCE(o.observed_at,'') FROM actor_delivery_attempts a LEFT JOIN actor_delivery_outcomes o ON o.delivery_id=a.delivery_id AND o.number=a.number WHERE a.delivery_id=? ORDER BY a.number`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActorDeliveryObservation
	for rows.Next() {
		h := ActorDeliveryObservation{Attempt: ActorDeliveryAttempt{IntentID: id}}
		var created, deadline, observed string
		if err := rows.Scan(&h.Attempt.Number, &h.Attempt.Mode, &created, &deadline, &h.Outcome, &h.Detail, &observed); err != nil {
			return nil, err
		}
		h.Attempt.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, ErrActorDeliveryConflict
		}
		h.Attempt.DeadlineAt, err = time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return nil, ErrActorDeliveryConflict
		}
		if observed != "" {
			h.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
			if err != nil {
				return nil, ErrActorDeliveryConflict
			}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func actorDeliveryBackoff(number int) time.Duration {
	delay := 5 * time.Second
	for i := 1; i < number && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

// ReserveActorDeliveryAttempt returns permission only after a FULL-sync commit.
// A missing or unconfirmed outcome from any send restricts retries to probes.
func (s *Store) ReserveActorDeliveryAttempt(ctx context.Context, id string, now, deadline time.Time, authorize func(context.Context) bool) (ActorDeliveryAttempt, error) {
	if now.IsZero() || !deadline.After(now) || deadline.Sub(now) > 10*time.Minute {
		return ActorDeliveryAttempt{}, ErrActorDeliveryConflict
	}
	var out ActorDeliveryAttempt
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrActorDeliveryDenied
		}
		intent, err := s.GetActorDelivery(ctx, id)
		if err != nil {
			return err
		}
		probe, err := s.actorDeliveryCurrent(ctx, intent.Input)
		if err != nil {
			return err
		}
		probe = probe || intent.ProbeOnly
		history, err := s.actorDeliveryHistory(ctx, id)
		if err != nil {
			return err
		}
		// У подготовленной инструкции без reservation ещё не было права на внешний send.
		if intent.Input.SourceKind == "step" && !intent.ProbeOnly && len(history) == 0 {
			probe = false
		}
		for _, h := range history {
			if h.Outcome == "delivered" || h.Outcome == "duplicate" {
				return ErrActorDeliveryComplete
			}
			if h.Attempt.Mode == "send" && h.Outcome != "not_sent" {
				probe = true
			}
		}
		schedule := scheduleActorDelivery(history)
		if schedule.Exhausted {
			return ErrActorDeliveryBudget
		}
		if now.Before(schedule.NotBefore) {
			return ErrActorDeliveryPending
		}
		mode := "send"
		if probe {
			mode = "probe"
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_delivery_attempts(delivery_id,number,mode,created_at,deadline_at) VALUES(?,?,?,?,?)`, id, len(history)+1, mode, now.UTC().Format(time.RFC3339Nano), deadline.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		history, err = s.actorDeliveryHistory(ctx, id)
		if err != nil {
			return err
		}
		out = history[len(history)-1].Attempt
		return nil
	})
	if err != nil {
		return ActorDeliveryAttempt{}, err
	}
	return out, nil
}

// RecordActorDeliveryOutcome retains a supervisor-observed result for one exact
// committed ticket. It does not reconcile or advance the source graph/request.
func (s *Store) RecordActorDeliveryOutcome(ctx context.Context, ticket ActorDeliveryAttempt, outcome, detail string, observed time.Time, authorize func(context.Context) bool) error {
	if outcome != "delivered" && outcome != "duplicate" && outcome != "not_sent" && outcome != "unconfirmed" || len(detail) > 512 || !utf8.ValidString(detail) || observed.IsZero() {
		return ErrActorDeliveryConflict
	}
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrActorDeliveryDenied
		}
		intent, err := s.GetActorDelivery(ctx, ticket.IntentID)
		if err != nil {
			return err
		}
		if _, err := s.actorDeliveryCurrent(ctx, intent.Input); err != nil {
			return err
		}
		history, err := s.actorDeliveryHistory(ctx, ticket.IntentID)
		if err != nil {
			return err
		}
		if ticket.Number < 1 || ticket.Number > len(history) {
			return ErrActorDeliveryConflict
		}
		h := history[ticket.Number-1]
		if h.Attempt.Mode != ticket.Mode || !h.Attempt.CreatedAt.Equal(ticket.CreatedAt) || !h.Attempt.DeadlineAt.Equal(ticket.DeadlineAt) {
			return ErrActorDeliveryConflict
		}
		if h.Outcome != "" {
			if h.Outcome == outcome && h.Detail == detail {
				return nil
			}
			return ErrActorDeliveryConflict
		}
		if h.Attempt.Mode == "probe" && outcome != "duplicate" && outcome != "unconfirmed" || observed.Before(h.Attempt.CreatedAt) {
			return ErrActorDeliveryConflict
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_delivery_outcomes(delivery_id,number,outcome,detail,observed_at) VALUES(?,?,?,?,?)`, ticket.IntentID, ticket.Number, outcome, detail, observed.UTC().Format(time.RFC3339Nano))
		return err
	})
}
