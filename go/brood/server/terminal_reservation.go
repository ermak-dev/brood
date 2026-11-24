package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/store"
)

// agentViewAuthority binds local transaction reads to the original web session.
func (s *Server) agentViewAuthority(ctx context.Context, v agentView) context.Context {
	return context.WithValue(ctx, commandSessionAuthorityKey{}, func(ctx context.Context) bool {
		if !time.Now().Before(v.expires) {
			return false
		}
		id, _, err := auth.NewSessions(s.store, 0).Load(ctx, v.session, time.Now())
		return err == nil && id == v.userID
	})
}

// beginAgentTerminal authorizes an explicitly selected writable terminal
// stream right before the upgrade request is sent to the agent. Ttyd has no
// acknowledged detach or effect outcome.
func (s *Server) beginAgentTerminal(r *http.Request, target proxy.Target) (proxy.UpgradeReservation, error) {
	agent, err := s.store.GetAgent(r.Context(), target.UID)
	if err != nil {
		return proxy.UpgradeReservation{}, err
	}
	if agent.Incarnation != target.Generation || r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !s.validProxySession(r) {
		return proxy.UpgradeReservation{}, app.ErrNotAllowed
	}
	user, ok := s.agentViewUser(r.Context())
	if !ok {
		return proxy.UpgradeReservation{}, app.ErrNotAllowed
	}
	ctx := s.webCommandContext(r, user.ID)
	if v, ok := r.Context().Value(agentViewKey{}).(agentView); ok {
		if v.readOnly || v.uid != agent.UID || v.generation != agent.Incarnation {
			return proxy.UpgradeReservation{}, app.ErrNotAllowed
		}
		ctx = s.agentViewAuthority(r.Context(), v)
	} else if r.URL.Query().Get("mode") != "rw" || len(r.URL.Query()["mode"]) != 1 {
		return proxy.UpgradeReservation{}, app.ErrNotAllowed
	}
	authorize := func(ctx context.Context) error {
		if err := s.authorizeSwarmManagerIntent(ctx, user.ID, agent.SwarmID); err != nil {
			return err
		}
		current, err := s.store.LookupAgent(ctx, agent.SwarmID, agent.Name)
		if err != nil {
			return err
		}
		if sw, err := s.store.GetSwarm(ctx, agent.SwarmID); err != nil || sw.State != "active" {
			return app.ErrNotAllowed
		}
		if current.UID != agent.UID || current.Incarnation != agent.Incarnation || current.Desired != "running" || current.Phase != "running" || current.Stage != "done" || current.Operation != "start" {
			return app.ErrNotAllowed
		}
		return nil
	}
	// Терминал — прямое управление владельца роя: он не занимает агента
	// бронью операции. Соединение живёт долго, и бронь на всё его время
	// закрыла бы агента для шины, а после обрыва осталась бы навсегда.
	// Право проверяется прямо перед отправкой запроса агенту.
	return proxy.UpgradeReservation{
		BeforeSubmit: func() (func(), error) {
			release, err := s.provider.effects.acquire(ctx, agent.UID)
			if err != nil {
				return nil, err
			}
			if err := s.store.WithTx(ctx, authorize); err != nil {
				release()
				return nil, err
			}
			return release, nil
		},
	}, nil
}

// serveReadonlyAgentView uses the guest's explicit read-only snapshot RPC, not
// ttyd, whose fixed attach executable always acquires a writable PTY attachment.
// Neither query parameters nor an Upgrade can turn this capability into a writer.
func (s *Server) serveReadonlyAgentView(w http.ResponseWriter, r *http.Request, v agentView, rest string) {
	w.Header().Set("X-Brood-Terminal-Mode", "ro")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if rest != "" || r.Header.Get("Upgrade") != "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.Error(w, "read-only terminal capability supports snapshots only; it cannot open a writable stream", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx, cancel := context.WithTimeout(s.agentViewAuthority(r.Context(), v), 30*time.Second)
	defer cancel()
	caller := "browser-readonly:" + store.NewID()
	invoke := func(ctx context.Context, args map[string]any) (command.Result, error) {
		args["name"] = v.agent
		raw, _ := json.Marshal(args)
		return s.executeOperatorSessionCommand(ctx, v.userID, v.swarm, caller, command.Invocation{Name: readonlyTerminalOperation, Arguments: raw})
	}
	opened, err := invoke(ctx, map[string]any{"action": "open", "mode": "ro"})
	var snapshot struct {
		TID    string `json:"tid"`
		Output string `json:"output"`
	}
	if err != nil || json.Unmarshal([]byte(opened.Text), &snapshot) != nil || snapshot.TID == "" {
		http.Error(w, "read-only terminal snapshot is unavailable", http.StatusBadGateway)
		return
	}
	tid := snapshot.TID
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = invoke(cleanup, map[string]any{"action": "close", "tid": tid, "seq": 2})
	}()
	result, err := invoke(ctx, map[string]any{"action": "read", "tid": tid, "seq": 1})
	if err != nil || json.Unmarshal([]byte(result.Text), &snapshot) != nil {
		http.Error(w, "read-only terminal snapshot is unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "Read-only terminal snapshot. Refresh to inspect again; input and resize are disabled.")
	fmt.Fprint(w, snapshot.Output)
}
