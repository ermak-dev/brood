package server

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

func TestPolicyNoticeReadsActualProjectedRequestFromDurableOutbox(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	ctx := context.Background()
	var readers []store.User
	for i, name := range []string{"alice", "bob"} {
		user, err := f.srv.store.GetUserByUsername(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, user)
		if name == "bob" {
			if err := f.srv.store.SetSwarmMember(ctx, f.swarm, user.ID, "viewer"); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now().UTC()
		code := "policy-notification-test-code-" + name
		if _, err := f.srv.store.IssueTelegramLinkCode(ctx, user.ID, code, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		chat := int64(300 + i)
		if _, err := f.srv.store.ConfirmTelegramLink(ctx, store.TelegramLinkConfirmation{Code: code, ChatID: chat, TelegramUserID: chat, Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("completion receipt rejected")
	}
	f.srv.steps.wait()
	candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range readers {
		notification, err := f.srv.store.BeginTelegramAttempt(ctx, user.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if notification.Kind != "request_opened" || notification.Event.Request != "policy:"+candidate.ID {
			t.Fatalf("notification did not address exact policy request: %+v", notification)
		}
		notice, err := f.srv.sender.notice(ctx, notification)
		if err != nil {
			t.Fatal(err)
		}
		contains(t, "stored policy notice", notice.text, "workflow policy requests confirmation", "storage", candidate.ID)
		if user.Username == "alice" && notice.markup == nil {
			t.Fatal("manager lost policy buttons")
		}
		if user.Username == "bob" && notice.markup != nil {
			t.Fatal("viewer got policy buttons")
		}
		if err := f.srv.store.SettleTelegramAttempt(ctx, notification.ID, store.TelegramOutcome{State: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatal("notification rendering repeated result observation")
	}
}

func TestPolicyNoticeNamesPolicyNotActorOrGraphQuestion(t *testing.T) {
	facts := telegramFacts{swarm: "delivery", request: &store.SwarmRequest{ID: "policy:gate", SwarmID: "swarm", Origin: "policy", Kind: "confirm", State: "open", Question: "Approve automatic transition?", Context: `{"candidate_id":"gate","human_rules":["storage"]}`}}
	notification := store.TelegramNotification{ID: 7, Kind: "request_opened", Manager: true, Event: store.TelegramEvent{Swarm: "swarm", Request: "policy:gate", Sequence: 1, Node: "build"}}
	notice := renderTelegramNotice(notification, facts, "https://brood.example")
	contains(t, "policy notice", notice.text, "workflow policy requests confirmation", "Approve automatic transition?", "storage", "https://brood.example/#/swarms/swarm")
	if strings.Contains(notice.text, "node build asks") || strings.Contains(notice.text, "actor ") {
		t.Fatal("policy notice misrepresented asker")
	}
	want := &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{{Text: "Yes", CallbackData: "a:7:0"}, {Text: "No", CallbackData: "a:7:1"}}}}
	if !reflect.DeepEqual(notice.markup, want) {
		t.Fatalf("policy manager keyboard: %+v", notice.markup)
	}
	notification.Manager = false
	viewer := renderTelegramNotice(notification, facts, "")
	if viewer.markup != nil || strings.Contains(viewer.text, "answer with a button") {
		t.Fatal("viewer got policy answer controls")
	}
	response := "no"
	facts.request.State, facts.request.Response, facts.answeredBy = "answered", &response, "alice"
	notification.Kind = "request_answered"
	answered := renderTelegramNotice(notification, facts, "")
	contains(t, "policy answered", answered.text, "alice answered the request of workflow policy", "no")
	if answered.markup != nil || strings.Contains(answered.text, "node build") {
		t.Fatal("policy answer became graph answer")
	}
}
