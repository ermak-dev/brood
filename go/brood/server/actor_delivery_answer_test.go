package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

type durableAnswerRelay struct {
	guestRelay
	mu          sync.RWMutex
	uid         string
	incarnation int64
	journal     string
}

func (h *durableAnswerRelay) DeliveryJournalID(uid string, incarnation int64) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if uid != h.uid || incarnation != h.incarnation {
		return ""
	}
	return h.journal
}
func (h *durableAnswerRelay) setJournal(id string) { h.mu.Lock(); defer h.mu.Unlock(); h.journal = id }

func durableAnswerServer(t *testing.T) (*requestServer, *durableAnswerRelay, *atomic.Int64) {
	t.Helper()
	rs := newRequestServer(t)
	rs.srv.actorDelivery.close()
	hub := &durableAnswerRelay{guestRelay: rs.srv.managed.hub, uid: rs.worker.UID, incarnation: rs.worker.Incarnation, journal: strings.Repeat("a", 64)}
	rs.srv.managed.hub = hub
	clock := &atomic.Int64{}
	clock.Store(time.Now().UTC().UnixNano())
	rs.srv.actorDelivery = newActorDeliveryService(rs.srv, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	return rs, hub, clock
}

func TestDurableAnswerCommitsIntentAndAttemptBeforeGuestCall(t *testing.T) {
	rs, hub, _ := durableAnswerServer(t)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	swarm := rs.master.SwarmID
	rs.reply = func() (relay.MessageResponse, error) {
		intent, err := rs.srv.store.GetActorDeliveryForSource(context.Background(), swarm, "answer", id)
		if err != nil {
			t.Errorf("guest called before retained intent: %v", err)
			return relay.MessageResponse{}, err
		}
		history, err := rs.srv.store.GetActorDeliveryHistory(context.Background(), intent.ID)
		if err != nil || len(history) != 1 || history[0].Attempt.Mode != "send" || history[0].Outcome != "" {
			t.Error("guest called before durable reservation")
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
	expectStatus(t, "durable answer", r, b, 200)
	if b["delivery"] != "delivered" || rs.deliveries.Load() != 1 {
		t.Fatalf("answer not reconciled: %v calls=%d", b, rs.deliveries.Load())
	}
	message := rs.messages[0]
	if message.DeliveryID == "" || message.JournalID != hub.DeliveryJournalID(rs.worker.UID, rs.worker.Incarnation) || message.Replay {
		t.Fatalf("message lost durable scope: %+v", message)
	}
}

func TestDurableAnswerLostReceiptRetriesOnlyReadOnlyProbeWithSamePayload(t *testing.T) {
	rs, _, clock := durableAnswerServer(t)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	swarm := rs.master.SwarmID
	rs.reply = func() (relay.MessageResponse, error) {
		if rs.deliveries.Load() == 1 {
			return relay.MessageResponse{}, errors.New("response lost")
		}
		return relay.MessageResponse{OK: true, Duplicate: true, Verdict: "duplicate; prior delivery confirmed"}, nil
	}
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
	expectStatus(t, "lost receipt", r, b, 200)
	if b["delivery"] != "unconfirmed" {
		t.Fatalf("lost receipt became success: %v", b)
	}
	path := "/api/swarms/" + swarm + "/requests/" + id + "/deliver"
	r, b, _ = rs.do(rs.alice, "POST", path, rs.csrf(rs.alice), nil)
	expectStatus(t, "early retry", r, b, 200)
	if rs.deliveries.Load() != 1 {
		t.Fatal("manual retry bypassed persisted backoff")
	}
	clock.Add(int64(time.Hour))
	r, b, _ = rs.do(rs.alice, "POST", path, rs.csrf(rs.alice), nil)
	expectStatus(t, "receipt probe", r, b, 200)
	if b["delivery"] != "delivered" || rs.deliveries.Load() != 2 {
		t.Fatalf("duplicate did not close unknown: %v calls=%d", b, rs.deliveries.Load())
	}
	first, probe := rs.messages[0], rs.messages[1]
	if first.DeliveryID == "" || first.DeliveryID != probe.DeliveryID || first.JournalID != probe.JournalID || !probe.Replay || first.Body != probe.Body || first.From != probe.From || first.To != probe.To || first.WaitSec != probe.WaitSec {
		t.Fatalf("retry changed identity or resent instead of probing: %+v %+v", first, probe)
	}
}

func TestDurableAnswerKnownNotSentRetriesSameIdentityWithoutProbe(t *testing.T) {
	rs, _, clock := durableAnswerServer(t)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	swarm := rs.master.SwarmID
	rs.fail.Store(true)
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
	expectStatus(t, "known not sent", r, b, 200)
	if b["delivery"] != "failed" || rs.deliveries.Load() != 1 {
		t.Fatalf("not-sent proof lost: %v", b)
	}
	rs.fail.Store(false)
	clock.Add(int64(time.Hour))
	r, b, _ = rs.do(rs.alice, "POST", "/api/swarms/"+swarm+"/requests/"+id+"/deliver", rs.csrf(rs.alice), nil)
	expectStatus(t, "safe retry", r, b, 200)
	if b["delivery"] != "delivered" || rs.deliveries.Load() != 2 {
		t.Fatalf("known safe retry failed: %v", b)
	}
	if rs.messages[0].DeliveryID == "" || rs.messages[0].DeliveryID != rs.messages[1].DeliveryID || rs.messages[1].Replay {
		t.Fatal("known not-sent retry changed logical identity or became a probe")
	}
}

func TestDurableAnswerNeverFallsBackAfterJournalChangesOrCapabilityDisappears(t *testing.T) {
	for _, journal := range []string{strings.Repeat("b", 64), ""} {
		t.Run(journal, func(t *testing.T) {
			rs, hub, clock := durableAnswerServer(t)
			id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
			swarm := rs.master.SwarmID
			rs.reply = func() (relay.MessageResponse, error) { return relay.MessageResponse{}, errors.New("response lost") }
			r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
			expectStatus(t, "lost receipt", r, b, 200)
			hub.setJournal(journal)
			clock.Add(int64(time.Hour))
			r, b, _ = rs.do(rs.alice, "POST", "/api/swarms/"+swarm+"/requests/"+id+"/deliver", rs.csrf(rs.alice), nil)
			expectStatus(t, "changed journal", r, b, 200)
			if rs.deliveries.Load() != 1 || b["delivery"] != "unconfirmed" {
				t.Fatalf("journal change authorized another delivery: %v calls=%d", b, rs.deliveries.Load())
			}
		})
	}
}
