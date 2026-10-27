// Фейковые зависимости прокси для проверок: авторизатор по списку,
// фиксированные рои, резолвер адресов и сборщик аудита.
package proxy

import (
	"context"
	"net/url"
	"sync"

	"brood.dev/go/brood/app"
)

// fakeAuthz — авторизатор по списку пользователь -> разрешён; может
// возвращать ошибку и считает вызовы (первый вызов — до соединения).
type fakeAuthz struct {
	mu      sync.Mutex
	allowed map[string]bool
	err     error
	calls   int
	lastAct app.Action
	lastRes app.Resource
}

func (f *fakeAuthz) Can(userID string, action app.Action, res app.Resource) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastAct = action
	f.lastRes = res
	if f.err != nil {
		return false, f.err
	}
	return f.allowed[userID], nil
}

func (f *fakeAuthz) set(userID string, allowed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowed[userID] = allowed
}

func (f *fakeAuthz) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeAuthz) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAuthz) lastAction() (app.Action, app.Resource) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAct, f.lastRes
}

// fakeSwarms — фиксированные рои по ID.
type fakeSwarms map[string]app.Swarm

func (f fakeSwarms) SwarmByID(id string) (app.Swarm, bool, error) {
	sw, ok := f[id]
	return sw, ok, nil
}

// fakeResolver — (atespace, actor) -> базовый URL; отсутствующая пара —
// ErrUnknownActor. Помнит вызовы: гейт обязан работать до резолва.
type fakeResolver struct {
	mu    sync.Mutex
	bases map[string]*url.URL
	calls []string
}

func (f *fakeResolver) ActorUpstream(atespace, actor string) (*url.URL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, atespace+"/"+actor)
	u, ok := f.bases[atespace+"/"+actor]
	if !ok {
		return nil, ErrUnknownActor
	}
	return u, nil
}

func (f *fakeResolver) set(atespace, actor string, base *url.URL) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bases == nil {
		f.bases = map[string]*url.URL{}
	}
	f.bases[atespace+"/"+actor] = base
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// auditLog — собирает события аудита из всех горутин.
type auditLog struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (a *auditLog) record(e AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

func (a *auditLog) snapshot() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEvent(nil), a.events...)
}

// fixedUser — CurrentUser с одним пользователем.
func fixedUser(id, username string) CurrentUser {
	return func(context.Context) (Identity, bool) {
		return Identity{ID: id, Username: username}, true
	}
}

// anonymousUser — CurrentUser без пользователя.
func anonymousUser() CurrentUser {
	return func(context.Context) (Identity, bool) { return Identity{}, false }
}
