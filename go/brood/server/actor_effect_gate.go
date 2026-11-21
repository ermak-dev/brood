package server

import (
	"context"
	"slices"
	"sync"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type actorEffectGateEntry struct {
	ready chan struct{}
	users int
}

type actorEffectGates struct {
	mu      sync.Mutex
	entries map[string]*actorEffectGateEntry
}

// acquire упорядочивает запись намерения над агентом и открытие канала
// эффекта, не удерживая транзакцию SQLite во время сетевой операции.
func (g *actorEffectGates) acquire(ctx context.Context, actors ...string) (func(), error) {
	keys := append([]string(nil), actors...)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, key := range keys {
		g.mu.Lock()
		if g.entries == nil {
			g.entries = make(map[string]*actorEffectGateEntry)
		}
		e := g.entries[key]
		if e == nil {
			e = &actorEffectGateEntry{ready: make(chan struct{}, 1)}
			e.ready <- struct{}{}
			g.entries[key] = e
		}
		e.users++
		g.mu.Unlock()
		drop := func() {
			g.mu.Lock()
			e.users--
			if e.users == 0 {
				delete(g.entries, key)
			}
			g.mu.Unlock()
		}
		select {
		case <-e.ready:
			releases = append(releases, func() { e.ready <- struct{}{}; drop() })
		case <-ctx.Done():
			drop()
			release()
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// fenceSessionOpen проверяет точный резерв в транспорте, а не перед
// потенциально задержанным адаптером. Запись намерения и открытие
// упорядочены; ожидание результата гостя не удерживает gate. Дополнительная проверка
// authorize читает только локальное состояние, без сетевой авторизации под gate.
func (m *managedProvider) fenceSessionOpen(ctx context.Context, op store.ActorOperation, authorize func(context.Context) error) context.Context {
	return relay.WithSessionOpenGuard(ctx, func(ctx context.Context) (func(), error) {
		release, err := m.effects.acquire(ctx, op.UID)
		if err != nil {
			return nil, err
		}
		current, err := m.server.store.CurrentActorOperation(ctx, op.UID)
		if err == nil && current != op {
			err = store.ErrCoordinationConflict
		}
		if err == nil && authorize != nil {
			err = authorize(ctx)
		}
		if err != nil {
			release()
			return nil, err
		}
		return release, nil
	})
}

type actorMessageScopeKey struct{}
type messageSubmissionKey struct{}

type messageSubmission struct {
	actors    []string
	authorize func(context.Context) error
}
