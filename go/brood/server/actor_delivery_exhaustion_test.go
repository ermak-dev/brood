package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

// BUG: The last reserved answer send can outlive a crash while the request still says failed.
// HYPOTHESIS: Recovery skips the exhausted ledger, and manual retry trusts the old projection.
// SOLUTION: Use retained send evidence before consulting or opening the guest channel.
func TestExhaustedAnswerDeliveryReconcilesRetainedSendEvidence(t *testing.T) {
	for _, test := range []struct {
		name, lastOutcome, want string
		manual                  bool
	}{
		{name: "missing receipt on recovery", want: "unconfirmed"},
		{name: "missing receipt on manual retry", want: "unconfirmed", manual: true},
		{name: "unknown retained before projection", lastOutcome: "unconfirmed", want: "unconfirmed"},
		{name: "every send known not sent", lastOutcome: "not_sent", want: "failed", manual: true},
		{name: "retained positive receipt", lastOutcome: "delivered", want: "delivered"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rs, hub, clock := durableAnswerServer(t)
			ctx := context.Background()
			swarm := rs.master.SwarmID
			id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
			rs.fail.Store(true)
			r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
			expectStatus(t, "known not sent", r, b, 200)
			for range store.MaxActorDeliveryAttempts - 2 {
				clock.Add(int64(time.Hour))
				if _, err := rs.srv.actorDelivery.recoverAnswers(ctx, ""); err != nil {
					t.Fatal(err)
				}
			}
			if got := rs.request(t, id); got.Delivery != "failed" || int(rs.deliveries.Load()) != store.MaxActorDeliveryAttempts-1 {
				t.Fatalf("fixture did not retain the preceding failed sends: %s, calls=%d", got.Delivery, rs.deliveries.Load())
			}
			intent, err := rs.srv.store.GetActorDeliveryForSource(ctx, swarm, "answer", id)
			if err != nil {
				t.Fatal(err)
			}
			authorize := func(context.Context) bool { return true }
			now := time.Unix(0, clock.Add(int64(time.Hour))).UTC()
			last, err := rs.srv.store.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), authorize)
			if err != nil || last.Number != store.MaxActorDeliveryAttempts || last.Mode != "send" {
				t.Fatalf("fixture did not reserve the last send: %+v, %v", last, err)
			}
			if test.lastOutcome != "" {
				if err := rs.srv.store.RecordActorDeliveryOutcome(ctx, last, test.lastOutcome, "fixture receipt", now, authorize); err != nil {
					t.Fatal(err)
				}
			}
			if test.want != "delivered" {
				views := rs.list(t, rs.alice, "?all=1")
				if len(views) != 1 || views[0]["delivery"] != test.want {
					t.Errorf("read before recovery did not publish %s: %v", test.want, views)
				}
				if current := rs.request(t, id).Delivery; current != "failed" {
					t.Errorf("read before recovery changed retained state to %s", current)
				}
			}
			if !test.manual || test.lastOutcome != "" {
				hub.setJournal("")
			}
			clock.Add(int64(time.Hour))
			if test.manual {
				path := "/api/swarms/" + swarm + "/requests/" + id + "/deliver"
				r, b, _ := rs.do(rs.alice, "POST", path, rs.csrf(rs.alice), nil)
				expectStatus(t, "exhausted manual retry", r, b, 200)
				if b["delivery"] != test.want {
					t.Errorf("manual retry reported delivery %v, want %s", b["delivery"], test.want)
				}
			} else if _, err := rs.srv.actorDelivery.recoverAnswers(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if current := rs.request(t, id).Delivery; current != test.want {
				t.Errorf("persisted delivery %s, want %s", current, test.want)
			}
			views := rs.list(t, rs.alice, "?all=1")
			if len(views) != 1 || views[0]["delivery"] != test.want {
				t.Fatalf("answer history did not publish %s: %v", test.want, views)
			}
			reason, _ := views[0]["deliveryError"].(string)
			if test.want == "delivered" && reason != "" || test.want != "delivered" && !strings.Contains(reason, "budget exhausted") {
				t.Fatalf("answer history published the wrong reason for %s: %q", test.want, reason)
			}
			history, err := rs.srv.store.GetActorDeliveryHistory(ctx, intent.ID)
			if err != nil || len(history) != store.MaxActorDeliveryAttempts || int(rs.deliveries.Load()) != store.MaxActorDeliveryAttempts-1 {
				t.Fatalf("recovery reserved or sent another attempt: entries=%d, calls=%d, err=%v", len(history), rs.deliveries.Load(), err)
			}
		})
	}
}
