package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// Виды вердикта о работе агента. Решения стоят на виде, а не на тексте:
// раздавать работу можно только агенту вида free.
const (
	workFree        = "free"
	workBusy        = "busy"
	workIdle        = "idle"
	workNeedsAction = "needs_action"
	workWaiting     = "waiting"
	workError       = "error"
	workOffline     = "offline"
	workUnknown     = "unknown"
)

// Панель опрашивает список раз в несколько секунд, а обмер клона — это Git в
// контейнере: список читает снимок не старше этого срока. Ростер команды
// agents спрашивает агентов заново — на нём стоит решение о раздаче.
const agentWorkTTL = 10 * time.Second

// Гость укладывает ответ в свой бюджет; дольше ждать нечего.
const agentWorkTimeout = 10 * time.Second

// Сколько агентов опрашивается разом.
const agentWorkParallel = 16

// agentWork is what a running agent does and what its clone holds. Activity
// is the verdict; Detail explains it in words. Uncommitted and Unpushed are
// nil when the clone was not measured.
type agentWork struct {
	Activity    string `json:"activity"`
	Detail      string `json:"detail,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Uncommitted *int   `json:"uncommitted,omitempty"`
	Unpushed    *int   `json:"unpushed,omitempty"`
	Model       string `json:"model,omitempty"`
}

type agentWorkEntry struct {
	generation int64
	work       agentWork
	at         time.Time
}

type agentWorkCache struct {
	mu      sync.Mutex
	entries map[string]agentWorkEntry
}

func (c *agentWorkCache) get(a store.ManagedActor) (agentWork, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[a.UID]
	if !ok || entry.generation != a.Incarnation || time.Since(entry.at) > agentWorkTTL {
		return agentWork{}, false
	}
	return entry.work, true
}

func (c *agentWorkCache) put(a store.ManagedActor, work agentWork) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]agentWorkEntry{}
	}
	for uid, entry := range c.entries {
		if time.Since(entry.at) > agentWorkTTL {
			delete(c.entries, uid)
		}
	}
	c.entries[a.UID] = agentWorkEntry{generation: a.Incarnation, work: work, at: time.Now()}
}

// workVerdict turns the guest's facts into the verdict. Free means that the
// session is idle and the clone holds no unsaved work, not that the session is
// silent; a clone that was not measured is unknown, never free.
func workVerdict(status relay.AgentStatus) agentWork {
	work := agentWork{Model: status.Model}
	var held []string
	if status.Clone == relay.CloneGit {
		work.Branch = status.Branch
		work.Uncommitted, work.Unpushed = &status.Uncommitted, &status.Unpushed
		// Раздельно: несохранённые файлы и неотправленные коммиты — разный
		// разговор с агентом.
		if status.Uncommitted > 0 {
			held = append(held, fmt.Sprintf("%d uncommitted", status.Uncommitted))
		}
		if status.Unpushed > 0 {
			held = append(held, fmt.Sprintf("%d unpushed", status.Unpushed))
		}
	}
	switch status.Session {
	case relay.SessionBusy:
		work.Activity = workBusy
	case relay.SessionNeedsAction:
		work.Activity, work.Detail = workNeedsAction, "the session waits for an answer in its terminal"
	case relay.SessionWaiting:
		work.Activity = workWaiting
	case relay.SessionError:
		work.Activity, work.Detail = workError, "the last turn failed: "+status.Error
	case relay.SessionOffline:
		work.Activity, work.Detail = workOffline, "no live Claude Code session"
	case relay.SessionIdle:
		switch {
		case status.Clone == relay.CloneUnknown:
			work.Activity, work.Detail = workUnknown, "the clone could not be measured"
		case len(held) > 0:
			// Сессия стоит, занят только клон: такого агента не ждут, а
			// спасают его работу.
			work.Activity, work.Detail = workIdle, strings.Join(held, ", ")
		default:
			work.Activity = workFree
		}
	default:
		work.Activity, work.Detail = workUnknown, "the session state is not recognized"
	}
	return work
}

// askAgentWork asks one running agent. No answer is unknown, not free.
func (m *managedProvider) askAgentWork(ctx context.Context, a store.ManagedActor) agentWork {
	hub, ok := m.hub.(sessionRelay)
	if !ok || !m.hub.Connected(a.UID, a.Incarnation) {
		return agentWork{Activity: workUnknown, Detail: "the agent is not connected"}
	}
	ctx, cancel := context.WithTimeout(ctx, agentWorkTimeout)
	defer cancel()
	reply, err := hub.CallSession(ctx, a.UID, a.Incarnation, relay.SessionRequest{Operation: relay.StatusOperation, Arguments: json.RawMessage(`{}`), Caller: "roster"})
	if err != nil {
		return agentWork{Activity: workUnknown, Detail: "the agent did not answer"}
	}
	if reply.Outcome == "unsupported" {
		return agentWork{Activity: workUnknown, Detail: "the agent runs an older brood-guest; reset it to update"}
	}
	var status relay.AgentStatus
	if !reply.OK || json.Unmarshal([]byte(reply.Text), &status) != nil {
		return agentWork{Activity: workUnknown, Detail: "the agent did not report its state"}
	}
	return workVerdict(status)
}

// agentWorks reports the work of the running agents among actors, by UID;
// the others have no entry. fresh asks every agent again instead of reading
// a recent answer. An agent without a saved model shows the model of its role.
func (m *managedProvider) agentWorks(ctx context.Context, actors []store.ManagedActor, fresh bool) map[string]agentWork {
	works := map[string]agentWork{}
	var mu sync.Mutex
	var pending sync.WaitGroup
	slots := make(chan struct{}, agentWorkParallel)
	for _, a := range actors {
		if a.Phase != "running" {
			continue
		}
		if !fresh {
			if work, ok := m.works.get(a); ok {
				works[a.UID] = work
				continue
			}
		}
		pending.Add(1)
		go func() {
			defer pending.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			work := m.askAgentWork(ctx, a)
			if work.Model == "" {
				if role, err := m.server.store.GetAgentRole(ctx, a.Role); err == nil {
					work.Model = role.Model
				}
			}
			m.works.put(a, work)
			mu.Lock()
			works[a.UID] = work
			mu.Unlock()
		}()
	}
	pending.Wait()
	return works
}
