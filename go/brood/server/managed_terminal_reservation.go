package server

import (
	"context"
	"encoding/json"
	"errors"
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

// actorViewAuthority binds local transaction reads to the original web session.
func (s *Server) actorViewAuthority(ctx context.Context, v actorView) context.Context {
	return context.WithValue(ctx, commandSessionAuthorityKey{}, func(ctx context.Context) bool {
		if !time.Now().Before(v.expires) {
			return false
		}
		id, _, err := auth.NewSessions(s.store, 0).Load(ctx, v.session, time.Now())
		return err == nil && id == v.userID
	})
}

// beginManagedTerminal reserves automatic scheduling for an explicitly selected
// writable stream, while owner intervention remains allowed during child work.
// Ttyd has no acknowledged detach/effect outcome; disconnect retains uncertainty.
func (s *Server) beginManagedTerminal(r *http.Request) (proxy.UpgradeReservation, error) {
	target, err := s.store.LookupManagedActor(r.Context(), r.PathValue("swarm"), r.PathValue("actor"))
	if errors.Is(err, store.ErrNotFound) {
		return proxy.UpgradeReservation{}, nil
	}
	if err != nil {
		return proxy.UpgradeReservation{}, err
	}
	if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !s.validProxySession(r) {
		return proxy.UpgradeReservation{}, app.ErrNotAllowed
	}
	user, ok := s.actorViewUser(r.Context())
	if !ok {
		return proxy.UpgradeReservation{}, app.ErrNotAllowed
	}
	ctx := s.webCommandContext(r, user.ID)
	if v, ok := r.Context().Value(actorViewKey{}).(actorView); ok {
		if v.readOnly || v.managedUID != target.UID || v.generation != target.Incarnation {
			return proxy.UpgradeReservation{}, app.ErrNotAllowed
		}
		ctx = s.actorViewAuthority(r.Context(), v)
	} else if r.URL.Query().Get("mode") != "rw" || len(r.URL.Query()["mode"]) != 1 {
		return proxy.UpgradeReservation{}, app.ErrNotAllowed
	}
	authorize := func(ctx context.Context) error {
		if err := s.authorizeSwarmManagerIntent(ctx, user.ID, target.SwarmID); err != nil {
			return err
		}
		current, err := s.store.LookupManagedActor(ctx, target.SwarmID, target.Name)
		if err != nil {
			return err
		}
		if sw, err := s.store.GetSwarm(ctx, target.SwarmID); err != nil || sw.State != "active" {
			return app.ErrNotAllowed
		}
		if current.UID != target.UID || current.Incarnation != target.Incarnation || current.Desired != "running" || current.Phase != "running" || current.Stage != "done" || current.Operation != "start" {
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
			release, err := s.managed.effects.acquire(ctx, target.UID)
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

// serveReadonlyActorView uses the guest's explicit read-only snapshot RPC, not
// ttyd, whose fixed attach executable always acquires a writable PTY attachment.
// Neither query parameters nor an Upgrade can turn this capability into a writer.
func (s *Server) serveReadonlyActorView(w http.ResponseWriter, r *http.Request, v actorView, rest string) {
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
	ctx, cancel := context.WithTimeout(s.actorViewAuthority(r.Context(), v), 30*time.Second)
	defer cancel()
	caller := "browser-readonly:" + store.NewID()
	invoke := func(ctx context.Context, args map[string]any) (command.Result, error) {
		args["name"] = v.actor
		raw, _ := json.Marshal(args)
		return s.executeOperatorSessionCommand(ctx, v.userID, v.swarm, caller, command.Invocation{Name: "session_terminal", Arguments: raw})
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
