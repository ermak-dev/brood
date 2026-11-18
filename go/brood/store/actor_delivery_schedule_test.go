package store

import (
	"context"
	"testing"
	"time"
)

func TestActorDeliveryScheduleUsesSamePersistedDeadlineAndBackoff(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	intent, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	schedule, err := s.GetActorDeliverySchedule(ctx, intent.ID)
	mustNil(t, err)
	if schedule.Confirmed || schedule.Exhausted || !schedule.NotBefore.IsZero() {
		t.Fatal("new intent was not immediately eligible")
	}
	now := time.Now().UTC()
	ticket, err := s.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Minute), allow)
	mustNil(t, err)
	schedule, err = s.GetActorDeliverySchedule(ctx, intent.ID)
	mustNil(t, err)
	if !schedule.NotBefore.Equal(ticket.DeadlineAt.Add(5 * time.Second)) {
		t.Fatal("pending call lost deadline before backoff")
	}
	observed := now.Add(time.Second)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, ticket, "unconfirmed", "reply lost", observed, allow))
	schedule, err = s.GetActorDeliverySchedule(ctx, intent.ID)
	mustNil(t, err)
	if !schedule.NotBefore.Equal(observed.Add(5 * time.Second)) {
		t.Fatal("observed outcome did not anchor backoff")
	}
}

func TestPendingActorDeliveryAnswersPagesRetainedSourcesOnly(t *testing.T) {
	ctx := context.Background()
	s, in := actorDeliveryFixture(t)
	first, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	next := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, next, "u", "", "use sqlite"))
	in.SourceID = next.ID
	second, err := s.PrepareActorDelivery(ctx, in, allow)
	mustNil(t, err)
	legacy := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, legacy, "u", "", "legacy answer"))
	page, err := s.PendingActorDeliveryAnswers(ctx, "", 1)
	mustNil(t, err)
	if len(page) != 1 {
		t.Fatal("bounded candidate page missing")
	}
	last := page[0].ID
	page2, err := s.PendingActorDeliveryAnswers(ctx, last, 1)
	mustNil(t, err)
	if len(page2) != 1 || page2[0].ID == last || page2[0].ID < last {
		t.Fatal("candidate cursor repeated or skipped retained source")
	}
	ids := map[string]bool{last: true, page2[0].ID: true}
	if !ids[first.ID] || !ids[second.ID] {
		t.Fatal("legacy answer became an automatic delivery candidate")
	}
	end, err := s.PendingActorDeliveryAnswers(ctx, page2[0].ID, 1)
	mustNil(t, err)
	if len(end) != 0 {
		t.Fatal("candidate cursor did not finish")
	}
	if _, err := s.PendingActorDeliveryAnswers(ctx, "", 0); err == nil {
		t.Fatal("unbounded/invalid candidate query accepted")
	}
}
