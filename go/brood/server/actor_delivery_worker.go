package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"brood.dev/go/brood/store"
)

func (d *actorDeliveryService) answerView(ctx context.Context, request store.SwarmRequest, view *swarmRequestJSON) error {
	if request.Origin != "actor" || request.State != "answered" || request.Delivery == "" || request.Delivery == "delivered" {
		return nil
	}
	intent, err := d.server.store.GetActorDeliveryForSource(ctx, request.SwarmID, "answer", request.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	schedule, err := d.server.store.GetActorDeliverySchedule(ctx, intent.ID)
	if err != nil {
		return err
	}
	if schedule.Exhausted {
		if schedule.UncertainSend {
			view.Delivery = "unconfirmed"
		}
		view.DeliveryError = durableAnswerBudget
	} else if request.Delivery == "unconfirmed" && !schedule.Confirmed {
		view.DeliveryError = durableAnswerUnconfirmed
	}
	return nil
}

const actorDeliveryPageSize = 16
const actorDeliveryParallelism = 4

func (d *actorDeliveryService) start(parent context.Context) {
	if d == nil {
		return
	}
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	if d.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	d.done = make(chan struct{})
	go func() {
		defer close(d.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		after, stepAfter, childAfter := "", "", ""
		for ctx.Err() == nil {
			if next, err := d.recoverAnswers(ctx, after); err == nil {
				after = next
			}
			if next, err := d.recoverSteps(ctx, stepAfter); err == nil {
				stepAfter = next
			}
			if next, err := d.recoverChildren(ctx, childAfter); err == nil {
				childAfter = next
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (d *actorDeliveryService) close() {
	if d == nil {
		return
	}
	d.lifecycle.Lock()
	cancel, done := d.cancel, d.done
	d.lifecycle.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (d *actorDeliveryService) recoverAnswers(ctx context.Context, after string) (string, error) {
	return d.recoverPage(ctx, after, d.server.store.PendingActorDeliveryAnswers, d.recoverAnswer)
}

func (d *actorDeliveryService) recoverSteps(ctx context.Context, after string) (string, error) {
	return d.recoverPage(ctx, after, d.server.store.PendingActorDeliverySteps, d.recoverStep)
}

func (d *actorDeliveryService) recoverPage(ctx context.Context, after string, list func(context.Context, string, int) ([]store.ActorDeliveryCandidate, error), process func(context.Context, store.ActorDeliveryCandidate)) (string, error) {
	candidates, err := list(ctx, after, actorDeliveryPageSize)
	if err != nil {
		return after, err
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, actorDeliveryParallelism)
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			workers.Wait()
			return after, ctx.Err()
		}
		workers.Add(1)
		go func(candidate store.ActorDeliveryCandidate) {
			defer workers.Done()
			defer func() { <-slots }()
			process(ctx, candidate)
		}(candidate)
	}
	workers.Wait()
	if len(candidates) < actorDeliveryPageSize {
		return "", ctx.Err()
	}
	return candidates[len(candidates)-1].ID, ctx.Err()
}

func (d *actorDeliveryService) recoverAnswer(parent context.Context, candidate store.ActorDeliveryCandidate) {
	s := d.server
	schedule, err := s.store.GetActorDeliverySchedule(parent, candidate.ID)
	if err != nil || !schedule.Confirmed && (d.now().Before(schedule.NotBefore) || schedule.Exhausted && !schedule.UncertainSend) {
		return
	}
	if !schedule.Confirmed && !schedule.Exhausted {
		intent, err := s.store.GetActorDelivery(parent, candidate.ID)
		if err != nil || s.managed.hub == nil || actorDeliveryJournal(s.managed.hub, intent.Input.ActorUID, intent.Input.Incarnation) != intent.Input.JournalID || !s.managed.hub.Connected(intent.Input.ActorUID, intent.Input.Incarnation) {
			return
		}
	}
	unlock, ok := s.deliveries.tryLock(candidate.SourceID)
	if !ok {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(parent, requestDeliveryTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	request, err := s.store.GetSwarmRequest(ctx, candidate.SwarmID, candidate.SourceID)
	if err != nil || request.Origin != "actor" || request.State != "answered" || request.Delivery != "pending" && request.Delivery != "failed" && request.Delivery != "unconfirmed" {
		return
	}
	author, err := s.store.GetUser(ctx, request.AnsweredBy)
	if err != nil || author.Disabled || s.store.CheckActorDeliveryManager(ctx, request.SwarmID, author.ID) != nil {
		return
	}
	// Фоновая доставка действует по сохранённому ответу, не от имени worker-admin.
	_, _, _ = d.answer(ctx, author, request)
}
