package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type actorDeliveryService struct {
	server    *Server
	now       func() time.Time
	lifecycle sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
}

func newActorDeliveryService(s *Server, now func() time.Time) *actorDeliveryService {
	if now == nil {
		now = time.Now
	}
	return &actorDeliveryService{server: s, now: now}
}

func actorDeliveryJournal(hub guestRelay, uid string, incarnation int64) string {
	observer, ok := hub.(interface{ DeliveryJournalID(string, int64) string })
	if !ok {
		return ""
	}
	return observer.DeliveryJournalID(uid, incarnation)
}

func actorDeliveryEvidence(mode string, response relay.MessageResponse, err error) (string, string) {
	outcome := "unconfirmed"
	if err == nil && response.Verdict != "" && !(response.OK && response.NotSent) && !(response.Duplicate && (!response.OK || response.NotSent)) {
		if mode == "probe" {
			if response.OK && response.Duplicate {
				outcome = "duplicate"
			}
		} else if mode == "send" {
			switch {
			case response.OK && response.Duplicate:
				outcome = "duplicate"
			case response.OK:
				outcome = "delivered"
			case response.NotSent:
				outcome = "not_sent"
			}
		}
	}
	details := map[string]string{"delivered": "recipient confirmed delivery", "duplicate": "retained duplicate confirmed", "not_sent": "recipient confirmed no session write", "unconfirmed": "recipient did not provide a usable delivery receipt"}
	return outcome, details[outcome]
}

func actorDeliveryMessage(intent store.ActorDeliveryIntent, mode string) (relay.MessageRequest, error) {
	var message relay.MessageRequest
	decoder := json.NewDecoder(strings.NewReader(intent.Input.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&message) != nil || message.DeliveryID != "" || message.JournalID != "" || message.Replay || message.To == "" || message.From == "" || message.Body == "" || message.WaitSec < 0 || message.WaitSec > 300 {
		return message, store.ErrActorDeliveryConflict
	}
	message.DeliveryID = intent.ID
	message.JournalID = intent.Input.JournalID
	message.Replay = mode == "probe"
	frame, err := json.Marshal(message)
	if err != nil || len(frame) > relay.MaxMessageBytes {
		return relay.MessageRequest{}, store.ErrActorDeliveryConflict
	}
	return message, nil
}

// Под effect gates допустимы только локальные чтения. Store authority читается
// в одном tx snapshot; relay проверяем после tx, поскольку его authorizeGuest
// открывает собственный Store context. Сетевого namespace lookup здесь нет.
func (d *actorDeliveryService) localAuthority(ctx context.Context, intent store.ActorDeliveryIntent, authorize func(context.Context) bool, peer bool) error {
	s := d.server
	err := s.store.WithTx(ctx, func(ctx context.Context) error {
		if !authorize(ctx) {
			return app.ErrNotAllowed
		}
		if err := d.checkEphemeralAnswerAuthority(ctx, intent); err != nil {
			return err
		}
		if err := s.store.CheckActorDeliveryCurrent(ctx, intent.ID); err != nil {
			return err
		}
		swarm, err := s.store.GetSwarm(ctx, intent.Input.SwarmID)
		if err != nil || swarm.AtespaceUID == nil || *swarm.AtespaceUID != intent.Input.AtespaceUID {
			return app.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	if peer {
		if s.managed.hub == nil || !s.managed.hub.Connected(intent.Input.ActorUID, intent.Input.Incarnation) || s.managed.deliverMessage == nil {
			return errRecipientOffline
		}
		if actorDeliveryJournal(s.managed.hub, intent.Input.ActorUID, intent.Input.Incarnation) != intent.Input.JournalID {
			return relay.ErrDeliveryJournalChanged
		}
	}
	return nil
}

func (d *actorDeliveryService) authority(ctx context.Context, intent store.ActorDeliveryIntent, authorize func(context.Context) bool, peer bool) error {
	if err := d.localAuthority(ctx, intent, authorize, peer); err != nil {
		return err
	}
	swarm, err := d.server.store.GetSwarm(ctx, intent.Input.SwarmID)
	if err != nil {
		return err
	}
	return d.server.checkSwarmNamespace(ctx, toAppSwarm(swarm))
}

func (d *actorDeliveryService) attempt(ctx context.Context, intent store.ActorDeliveryIntent, authorize func(context.Context) bool) (string, error) {
	s := d.server
	if _, err := s.store.ActorDeliveryInboxStatus(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		// Native callers must neither send nor project an outcome once custody
		// transport is pinned (or when the selection cannot be read safely).
		return "", errors.Join(errActorDeliveryInboxProtected, err)
	}
	history, err := s.store.GetActorDeliveryHistory(ctx, intent.ID)
	if err != nil {
		return "", err
	}
	for _, entry := range history {
		if entry.Outcome == "delivered" || entry.Outcome == "duplicate" {
			// Сохранённая квитанция не требует нового канала к гостю.
			if err := d.authority(ctx, intent, authorize, false); err != nil {
				return "", err
			}
			return "duplicate", s.store.ReconcileActorDelivery(ctx, intent.ID, authorize)
		}
	}
	if intent.Input.SourceKind == "answer" && len(history) >= store.MaxActorDeliveryAttempts {
		return "", store.ErrActorDeliveryBudget
	}
	if err := d.authority(ctx, intent, authorize, true); err != nil {
		return "", err
	}
	// Проверяем размер и для будущего probe до резервирования внешнего эффекта.
	if _, err := actorDeliveryMessage(intent, "probe"); err != nil {
		return "", err
	}
	now := d.now().UTC()
	ticket, err := s.store.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(requestDeliveryTimeout), authorize)
	if err != nil {
		return "", err
	}
	message, err := actorDeliveryMessage(intent, ticket.Mode)
	if err != nil {
		return "", err
	}
	actors, err := d.deliveryEffectActors(ctx, intent)
	if err != nil {
		return "", err
	}
	// Namespace preflight остаётся снаружи; delayed adapter обязан передать этот
	// context в настоящий deliverViaHub. Там acquire-many сортирует оба UID и
	// проверяет точный child/proof/owner/source внутри локальной транзакции.
	check := func(ctx context.Context) error { return d.localAuthority(ctx, intent, authorize, true) }
	var response relay.MessageResponse
	if err = d.authority(ctx, intent, authorize, true); err == nil {
		ctx = context.WithValue(ctx, messageSubmissionKey{}, messageSubmission{actors: actors, authorize: check})
		response, err = s.managed.deliverMessage(ctx, intent.Input.ActorUID, intent.Input.Incarnation, message)
	}
	outcome, detail := actorDeliveryEvidence(ticket.Mode, response, err)
	book, cancel := bookkeeping(ctx)
	defer cancel()
	if scopeErr := d.authority(book, intent, authorize, false); scopeErr != nil {
		return "unconfirmed", scopeErr
	}
	if recordErr := s.store.RecordActorDeliveryOutcome(book, ticket, outcome, detail, d.now().UTC(), authorize); recordErr != nil {
		return "unconfirmed", recordErr
	}
	if outcome == "delivered" || outcome == "duplicate" {
		return outcome, s.store.ReconcileActorDelivery(book, intent.ID, authorize)
	}
	return outcome, nil
}

const durableAnswerUnconfirmed = "delivery is unconfirmed; only a read-only receipt probe is permitted"
const durableAnswerBudget = "delivery attempt budget exhausted; no further automatic attempts are allowed"

func (d *actorDeliveryService) answer(ctx context.Context, user store.User, current store.SwarmRequest) (store.SwarmRequest, string, bool) {
	s := d.server
	scope := store.ActorDeliveryIntent{Input: store.ActorDeliveryInput{SourceKind: "answer", SwarmID: current.SwarmID, SourceID: current.ID}}
	local := func(ctx context.Context) bool {
		return s.store.CheckActorDeliveryManager(ctx, current.SwarmID, user.ID) == nil && d.checkEphemeralAnswerAuthority(ctx, scope) == nil
	}
	intent, err := s.store.GetActorDeliveryForSource(ctx, current.SwarmID, "answer", current.ID)
	if errors.Is(err, store.ErrNotFound) {
		actor, resolveErr := s.requestAsker(ctx, current)
		if resolveErr != nil {
			return current, "", false
		}
		journal := actorDeliveryJournal(s.managed.hub, actor.UID, actor.Incarnation)
		if journal == "" {
			return current, "", false
		}
		author := "operator"
		if original, err := s.store.GetUser(ctx, current.AnsweredBy); err == nil {
			author = original.Username
		}
		sender, lookupErr := s.store.GetUser(ctx, user.ID)
		if lookupErr != nil || sender.Disabled {
			return current, "delivery authorization changed", true
		}
		payload, encodeErr := json.Marshal(relay.MessageRequest{To: actor.Name, From: "operator:" + sender.Username, Body: operatorMessageBody(requestAnswerMessage(current, author))})
		if encodeErr != nil {
			return current, "delivery unavailable", true
		}
		intent, err = s.store.PrepareActorDelivery(ctx, store.ActorDeliveryInput{SwarmID: current.SwarmID, SourceKind: "answer", SourceID: current.ID, ActorUID: actor.UID, Incarnation: actor.Incarnation, AtespaceUID: actor.AtespaceUID, JournalID: journal, Payload: string(payload)}, local)
	}
	if err != nil {
		return current, "delivery unavailable", true
	}
	outcome, err := d.attempt(ctx, intent, local)
	if errors.Is(err, errActorDeliveryInboxProtected) {
		return current, errActorDeliveryInboxProtected.Error(), true
	}
	if errors.Is(err, store.ErrActorDeliveryPending) {
		return current, "delivery retry is waiting for persisted backoff", true
	}
	if errors.Is(err, store.ErrActorDeliveryBudget) {
		book, cancel := bookkeeping(ctx)
		defer cancel()
		updated, recordErr := s.store.ReconcileExhaustedActorDeliveryAnswer(book, intent.ID, d.now().UTC(), local)
		if recordErr == nil {
			current = updated
		} else if !errors.Is(recordErr, store.ErrActorDeliveryDenied) && !errors.Is(recordErr, store.ErrActorDeliveryConflict) && !errors.Is(recordErr, context.Canceled) {
			requestLogger(book).Error("exhausted swarm answer delivery was not reconciled", "request", current.ID, "err", recordErr.Error())
		}
		return current, durableAnswerBudget, true
	}
	if err == nil && (outcome == "delivered" || outcome == "duplicate") {
		updated, readErr := s.store.GetSwarmRequest(ctx, current.SwarmID, current.ID)
		if readErr != nil {
			return current, "delivery unavailable", true
		}
		return updated, "", true
	}
	state, reason := "unconfirmed", durableAnswerUnconfirmed
	if outcome == "not_sent" && err == nil {
		state, reason = "failed", "the answer was not sent: the actor session did not receive it"
	}
	if errors.Is(err, relay.ErrDeliveryJournalChanged) {
		reason = "delivery is unconfirmed: the receiver journal changed; no message was resent"
	}
	book, cancel := bookkeeping(ctx)
	defer cancel()
	updated, recordErr := s.store.RecordSwarmRequestDelivery(book, current.SwarmID, current.ID, state)
	if recordErr != nil {
		return current, "delivery unavailable", true
	}
	if state == "failed" && updated.Delivery == "unconfirmed" {
		reason = durableAnswerUnconfirmed
	}
	return updated, reason, true
}
