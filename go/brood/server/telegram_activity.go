package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/telegram"
)

const (
	telegramTypingInterval = 4 * time.Second
	telegramActivityLease  = 20 * time.Second
	telegramFocusLease     = 40 * time.Second
)

type telegramTurn struct {
	guest   guestRuntime
	event   guestSessionEvent
	expires time.Time
}

type telegramFocus struct {
	chat              telegramChat
	target, selection string
	expires           time.Time
}

type telegramTypingTarget struct {
	focus telegramFocus
	turn  telegramTurn
}

type telegramActivity struct {
	server  *Server
	mu      sync.Mutex
	turns   map[string]telegramTurn
	focus   map[string]telegramFocus
	next    map[string]time.Time
	sending map[string]bool
	slots   chan struct{}
	done    chan struct{}
}

func newTelegramActivity(s *Server) *telegramActivity {
	return &telegramActivity{server: s, turns: map[string]telegramTurn{}, focus: map[string]telegramFocus{}, next: map[string]time.Time{}, sending: map[string]bool{}, slots: make(chan struct{}, 8), done: make(chan struct{})}
}

func (a *telegramActivity) update(g guestRuntime, event guestSessionEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	old, exists := a.turns[g.uid]
	if exists && g.generation < old.guest.generation {
		return
	}
	if exists && g.generation == old.guest.generation {
		same := old.event.SessionID == event.SessionID && old.event.TurnID == event.TurnID
		if (event.State != "working" && !same) || (event.State == "working" && same && old.event.State == "error") {
			return
		}
	}
	a.turns[g.uid] = telegramTurn{guest: g, event: event, expires: time.Now().Add(telegramActivityLease)}
}

func (s *Server) noteTelegramDelivery(chat telegramChat, actorUID string) {
	if s.telegramActivity == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	link, current, ok := s.liveTelegramLink(ctx, chat.userID)
	if !ok || current.session != chat.session {
		return
	}
	a := s.telegramActivity
	a.mu.Lock()
	defer a.mu.Unlock()
	a.focus[chat.userID] = telegramFocus{chat: chat, target: actorUID, selection: link.DefaultActorUID, expires: time.Now().Add(telegramFocusLease)}
}

func (s *Server) clearTelegramFocus(chat telegramChat) {
	a := s.telegramActivity
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.focus[chat.userID].chat.session == chat.session {
		delete(a.focus, chat.userID)
	}
}

func (a *telegramActivity) start(ctx context.Context) {
	go func() {
		defer close(a.done)
		var pending sync.WaitGroup
		defer pending.Wait()
		tick := time.NewTicker(telegramTypingInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				a.round(ctx, &pending)
			}
		}
	}()
}

func (a *telegramActivity) wait() { <-a.done }

func (a *telegramActivity) round(ctx context.Context, pending *sync.WaitGroup) {
	now := time.Now()
	a.mu.Lock()
	turns := map[string]telegramTurn{}
	for uid, t := range a.turns {
		if !now.Before(t.expires) {
			delete(a.turns, uid)
		} else if t.event.State == "working" {
			turns[uid] = t
		}
	}
	focuses := map[string]telegramFocus{}
	for user, f := range a.focus {
		if !now.Before(f.expires) {
			delete(a.focus, user)
		} else {
			focuses[user] = f
		}
	}
	for session, next := range a.next {
		if !now.Before(next) {
			delete(a.next, session)
		}
	}
	a.mu.Unlock()
	targets := map[string]telegramTypingTarget{}
	for user, focus := range focuses {
		turn, ok := turns[focus.target]
		if ok {
			targets[user] = telegramTypingTarget{focus: focus, turn: turn}
		}
	}
	for _, turn := range turns {
		user := turn.guest.swarm.OwnerID
		if _, targeted := targets[user]; targeted {
			continue
		}
		link, chat, ok := a.server.liveTelegramLink(ctx, user)
		if ok && link.DefaultActorUID == turn.guest.uid {
			targets[user] = telegramTypingTarget{focus: telegramFocus{chat: chat, target: turn.guest.uid, selection: link.DefaultActorUID}, turn: turn}
		}
	}
	for _, target := range targets {
		if !a.claim(now, target.focus.chat.session) {
			continue
		}
		pending.Go(func() { a.send(ctx, target) })
	}
}

func (a *telegramActivity) claim(now time.Time, session string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending[session] || now.Before(a.next[session]) {
		return false
	}
	select {
	case a.slots <- struct{}{}:
	default:
		return false
	}
	a.sending[session] = true
	return true
}

func (a *telegramActivity) send(parent context.Context, target telegramTypingTarget) {
	f, t := target.focus, target.turn
	var delay time.Duration
	defer func() {
		a.mu.Lock()
		delete(a.sending, f.chat.session)
		if delay > 0 {
			a.next[f.chat.session] = time.Now().Add(delay)
		}
		a.mu.Unlock()
		<-a.slots
	}()
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	s := a.server
	link, chat, ok := s.liveTelegramLink(ctx, f.chat.userID)
	if !ok || chat.session != f.chat.session || link.DefaultActorUID != f.selection {
		return
	}
	allowed, err := s.app.Can(chat.userID, app.ActionManageSwarm, app.SwarmRes(t.guest.swarm.ID))
	if err != nil || !allowed || !s.guestRuntimeCurrent(ctx, t.guest) || !s.managed.hub.Connected(t.guest.uid, t.guest.generation) {
		return
	}
	token, err := s.store.TelegramBotToken(ctx, chat.userID, s.telegram.install)
	if err != nil || token == "" {
		return
	}
	client, err := telegram.NewClient(token, s.telegram.client)
	if err != nil {
		return
	}
	defer client.Close()
	a.mu.Lock()
	current, exists := a.turns[t.guest.uid]
	busy := exists && current.event == t.event && current.guest.generation == t.guest.generation && time.Now().Before(current.expires)
	focus := a.focus[chat.userID]
	if busy && !f.expires.IsZero() {
		busy = focus == f
		if busy {
			focus.expires = time.Now().Add(telegramFocusLease)
			a.focus[chat.userID] = focus
		}
	}
	a.mu.Unlock()
	if !busy {
		return
	}
	if err = client.SendTyping(ctx, chat.chatID); err != nil {
		var refusal *telegram.Error
		if errors.As(err, &refusal) {
			delay = refusal.RetryAfter
		}
	}
}
