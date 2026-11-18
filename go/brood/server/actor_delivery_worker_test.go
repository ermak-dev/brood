package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func makeUnknownDurableAnswer(t *testing.T, rs *requestServer) string {
	t.Helper()
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	rs.reply = func() (relay.MessageResponse, error) { return relay.MessageResponse{}, errors.New("reply lost") }
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(rs.master.SwarmID, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
	expectStatus(t, "unknown answer", r, b, 200)
	if b["delivery"] != "unconfirmed" || rs.deliveries.Load() != 1 {
		t.Fatalf("fixture did not create one uncertain send: %v", b)
	}
	return id
}

func TestDeliveryWorkerTryLockYieldsAndReleasesSharedSerializer(t *testing.T) {
	var locks deliveryLocks
	manual := locks.lock("source")
	if release, ok := locks.tryLock("source"); ok {
		release()
		t.Fatal("background worker acquired foreground source")
	}
	manual()
	background, ok := locks.tryLock("source")
	if !ok {
		t.Fatal("idle source could not be recovered")
	}
	if release, ok := locks.tryLock("source"); ok {
		release()
		t.Fatal("second worker acquired same source")
	}
	background()
	if len(locks.locks) != 0 {
		t.Fatal("background source lock leaked")
	}
}

func TestAutomaticAnswerRecoveryWaitsForBackoffAndUsesOriginalIdentity(t *testing.T) {
	rs, _, clock := durableAnswerServer(t)
	id := makeUnknownDurableAnswer(t, rs)
	_, err := rs.srv.actorDelivery.recoverAnswers(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rs.deliveries.Load() != 1 {
		t.Fatal("background recovery bypassed persisted backoff")
	}
	clock.Add(int64(time.Hour))
	rs.reply = func() (relay.MessageResponse, error) {
		return relay.MessageResponse{OK: true, Duplicate: true, Verdict: "confirmed"}, nil
	}
	_, err = rs.srv.actorDelivery.recoverAnswers(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rs.request(t, id).Delivery != "delivered" || rs.deliveries.Load() != 2 {
		t.Fatal("background duplicate did not reconcile the original answer")
	}
	if !rs.messages[1].Replay || rs.messages[1].DeliveryID != rs.messages[0].DeliveryID || rs.messages[1].Body != rs.messages[0].Body {
		t.Fatal("background recovery repeated a model write or changed its payload")
	}
}

func TestAutomaticAnswerRecoveryYieldsToForegroundDeliveryLock(t *testing.T) {
	rs, _, clock := durableAnswerServer(t)
	id := makeUnknownDurableAnswer(t, rs)
	clock.Add(int64(time.Hour))
	unlock := rs.srv.deliveries.lock(id)
	done := make(chan error, 1)
	go func() { _, err := rs.srv.actorDelivery.recoverAnswers(context.Background(), ""); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		unlock()
		t.Fatal("background recovery blocked behind a manual request")
	}
	unlock()
	if rs.deliveries.Load() != 1 {
		t.Fatal("background recovery raced a foreground delivery")
	}
}

func TestAutomaticAnswerRecoveryDoesNotUseWorkerAdminAfterAuthorDisabled(t *testing.T) {
	rs, _, clock := durableAnswerServer(t)
	makeUnknownDurableAnswer(t, rs)
	clock.Add(int64(time.Hour))
	ctx := context.Background()
	author, err := rs.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.srv.store.CreateUser(ctx, store.User{ID: "recovery-admin", Username: "recovery-admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if err := rs.srv.store.SetUserDisabled(ctx, author.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.srv.actorDelivery.recoverAnswers(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if rs.deliveries.Load() != 1 {
		t.Fatal("background worker substituted its own administrator authority")
	}
}

func TestAutomaticAnswerRecoveryStopsAtBudgetAndPublishesDurableReason(t *testing.T) {
	rs, _, clock := durableAnswerServer(t)
	id := makeUnknownDurableAnswer(t, rs)
	rs.reply = func() (relay.MessageResponse, error) {
		return relay.MessageResponse{Verdict: "retained receipt unknown"}, nil
	}
	for number := 2; number <= store.MaxActorDeliveryAttempts; number++ {
		clock.Add(int64(time.Hour))
		if _, err := rs.srv.actorDelivery.recoverAnswers(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if int(rs.deliveries.Load()) != number {
			t.Fatalf("attempt%d was not retained", number)
		}
	}
	clock.Add(int64(time.Hour))
	if _, err := rs.srv.actorDelivery.recoverAnswers(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if int(rs.deliveries.Load()) != store.MaxActorDeliveryAttempts || rs.request(t, id).Delivery != "unconfirmed" {
		t.Fatal("budget exhaustion reset unknown delivery")
	}
	views := rs.list(t, rs.alice, "?all=1")
	if len(views) != 1 {
		t.Fatal("answer disappeared from history")
	}
	reason, _ := views[0]["deliveryError"].(string)
	if !strings.Contains(reason, "budget exhausted") {
		t.Fatalf("durable exhaustion reason was not visible: %v", views[0])
	}
}

func TestAutomaticAnswerRecoveryDoesNotCreateIntentsForLegacyGuests(t *testing.T) {
	rs := newRequestServer(t)
	makeUnknownDurableAnswer(t, rs)
	if _, err := rs.srv.actorDelivery.recoverAnswers(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if rs.deliveries.Load() != 1 {
		t.Fatal("legacy guest received an automatic retry")
	}
}

func TestAutomaticAnswerRecoveryStartsWithSavedUnknownAndStopsInFlightProbe(t *testing.T) {
	rs, _, clock := durableAnswerServer(t)
	makeUnknownDurableAnswer(t, rs)
	clock.Add(int64(time.Hour))
	entered := make(chan struct{})
	rs.reply = func() (relay.MessageResponse, error) { return relay.MessageResponse{Verdict: "unknown"}, nil }
	previous := rs.srv.managed.deliverMessage
	rs.srv.managed.deliverMessage = func(ctx context.Context, uid string, incarnation int64, message relay.MessageRequest) (relay.MessageResponse, error) {
		if !message.Replay {
			t.Error("startup recovery attempted another session write")
		}
		close(entered)
		<-ctx.Done()
		return relay.MessageResponse{}, ctx.Err()
	}
	defer func() { rs.srv.managed.deliverMessage = previous }()
	rs.srv.actorDelivery.start(context.Background())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("startup recovery never scanned the saved unknown intent")
	}
	closed := make(chan struct{})
	go func() { rs.srv.actorDelivery.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(7 * time.Second):
		t.Fatal("worker shutdown did not cancel an in-flight receipt probe")
	}
}
